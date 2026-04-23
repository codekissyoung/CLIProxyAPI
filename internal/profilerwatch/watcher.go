package profilerwatch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	log "github.com/sirupsen/logrus"
)

const runtimeRefreshInterval = 30 * time.Second

type captureMetadata struct {
	CapturedAt         time.Time        `yaml:"captured-at"`
	Trigger            string           `yaml:"trigger"`
	ObservedCPUPercent float64          `yaml:"observed-cpu-percent"`
	SamplingInterval   string           `yaml:"sampling-interval"`
	PID                int              `yaml:"pid"`
	Comm               string           `yaml:"comm"`
	Cmdline            string           `yaml:"cmdline,omitempty"`
	Runtime            *ResolvedRuntime `yaml:"runtime,omitempty"`
	CaptureOutputDir   string           `yaml:"capture-output-dir"`
	CPUProfileSeconds  int              `yaml:"cpu-profile-seconds"`
	Cooldown           string           `yaml:"cooldown"`
	MaxCapturesPerHour int              `yaml:"max-captures-per-hour"`
	SustainedThreshold float64          `yaml:"sustained-threshold-percent"`
	BurstThreshold     float64          `yaml:"burst-threshold-percent"`
	SustainedSamples   int              `yaml:"sustained-consecutive-samples"`
	Errors             []string         `yaml:"errors,omitempty"`
}

// Watcher continuously samples cliproxyapi CPU usage and captures evidence when thresholds are exceeded.
type Watcher struct {
	cfg *Config

	mu                 sync.Mutex
	sampler            *CPUSampler
	lastRuntimeRefresh time.Time
	runtime            *ResolvedRuntime
	captureHistory     []time.Time
	lastCaptureAt      time.Time
	consecutiveHigh    int
	lastLogByKey       map[string]time.Time
	missingProcess     bool
}

// New constructs a watcher instance.
func New(cfg *Config) (*Watcher, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	return &Watcher{
		cfg:          cfg,
		sampler:      NewCPUSampler(),
		lastLogByKey: make(map[string]time.Time),
	}, nil
}

// Run starts the long-running watcher loop until the context is canceled.
func (w *Watcher) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.cfg.Capture.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create capture output dir: %w", err)
	}
	if err := w.refreshRuntime(true); err != nil {
		w.logRateLimited("runtime_refresh", time.Minute, log.WarnLevel, "failed to resolve runtime on startup: %v", err)
	} else if w.runtime != nil {
		log.WithFields(log.Fields{
			"pprof_url":    w.runtime.PprofBaseURL,
			"request_log":  w.runtime.RequestLogPath,
			"service_port": w.runtime.ServicePort,
		}).Info("cliproxy profiler watcher resolved runtime target")
	}
	if err := w.applyRetention(); err != nil {
		w.logRateLimited("retention_startup", time.Minute, log.WarnLevel, "failed to apply retention on startup: %v", err)
	}

	ticker := time.NewTicker(w.cfg.Sampling.Interval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.step(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.logRateLimited("step_error", 30*time.Second, log.WarnLevel, "watcher iteration failed: %v", err)
			}
		}
	}
}

// Check validates the current runtime target and pprof reachability.
func (w *Watcher) Check(ctx context.Context) error {
	if err := w.refreshRuntime(true); err != nil {
		return err
	}
	process, err := FindProcess(ProcessMatcher{
		PID:              w.cfg.Target.PID,
		Comm:             w.cfg.Target.Comm,
		CommandSubstring: w.cfg.Target.CommandSubstring,
	})
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"pid":     process.PID,
		"comm":    process.Comm,
		"cmdline": process.Cmdline,
	}).Info("cliproxy profiler watcher matched target process")

	if w.runtime == nil {
		return fmt.Errorf("runtime resolution returned nil result")
	}
	log.WithFields(log.Fields{
		"pprof_url":     w.runtime.PprofBaseURL,
		"pprof_enabled": w.runtime.PprofEnabled,
		"request_log":   w.runtime.RequestLogPath,
		"service_port":  w.runtime.ServicePort,
	}).Info("cliproxy profiler watcher resolved target runtime")

	if strings.TrimSpace(w.runtime.PprofBaseURL) == "" {
		return fmt.Errorf("pprof endpoint is not configured or not enabled")
	}
	client := &http.Client{Timeout: w.cfg.Capture.RequestTimeout.Duration}
	resp, err := client.Get(w.runtime.PprofBaseURL + "/")
	if err != nil {
		return fmt.Errorf("probe pprof index: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close pprof probe response body")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pprof probe returned status %s", resp.Status)
	}
	log.Info("cliproxy profiler watcher successfully probed pprof endpoint")
	return nil
}

func (w *Watcher) step(ctx context.Context) error {
	if err := w.refreshRuntime(false); err != nil {
		w.logRateLimited("runtime_refresh", time.Minute, log.WarnLevel, "failed to refresh runtime target: %v", err)
	}

	process, err := FindProcess(ProcessMatcher{
		PID:              w.cfg.Target.PID,
		Comm:             w.cfg.Target.Comm,
		CommandSubstring: w.cfg.Target.CommandSubstring,
	})
	if err != nil {
		w.mu.Lock()
		w.consecutiveHigh = 0
		w.missingProcess = true
		w.mu.Unlock()
		w.logRateLimited("missing_process", time.Minute, log.WarnLevel, "target process not found: %v", err)
		return nil
	}

	w.mu.Lock()
	if w.missingProcess {
		log.WithFields(log.Fields{"pid": process.PID, "comm": process.Comm}).Info("target process appeared again")
	}
	w.missingProcess = false
	w.mu.Unlock()

	sample, ok, err := w.sampler.Sample(*process)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	trigger := w.evaluateTrigger(*sample)
	if trigger == "" {
		return nil
	}

	if !w.allowCapture(time.Now()) {
		w.logRateLimited("capture_skipped", 30*time.Second, log.InfoLevel, "capture trigger %s skipped by cooldown/rate limit (cpu=%.2f%%, pid=%d)", trigger, sample.CPUPercent, sample.Process.PID)
		return nil
	}

	if err := w.capture(ctx, *sample, trigger); err != nil {
		return err
	}
	return nil
}

func (w *Watcher) refreshRuntime(force bool) error {
	w.mu.Lock()
	stale := force || w.runtime == nil || time.Since(w.lastRuntimeRefresh) >= runtimeRefreshInterval
	w.mu.Unlock()
	if !stale {
		return nil
	}
	resolved, err := ResolveRuntime(w.cfg)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.runtime = resolved
	w.lastRuntimeRefresh = time.Now()
	w.mu.Unlock()
	return nil
}

func (w *Watcher) evaluateTrigger(sample CPUSample) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if sample.CPUPercent >= w.cfg.Sampling.BurstThresholdPercent {
		w.consecutiveHigh = 0
		return "burst"
	}
	if sample.CPUPercent >= w.cfg.Sampling.SustainedThresholdPercent {
		w.consecutiveHigh++
		if w.consecutiveHigh >= w.cfg.Sampling.SustainedConsecutiveSamples {
			w.consecutiveHigh = 0
			return "sustained"
		}
		return ""
	}
	w.consecutiveHigh = 0
	return ""
}

func (w *Watcher) allowCapture(now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.lastCaptureAt.IsZero() && w.cfg.Capture.Cooldown.Duration > 0 && now.Sub(w.lastCaptureAt) < w.cfg.Capture.Cooldown.Duration {
		return false
	}
	cutoff := now.Add(-time.Hour)
	filtered := w.captureHistory[:0]
	for _, ts := range w.captureHistory {
		if ts.After(cutoff) {
			filtered = append(filtered, ts)
		}
	}
	w.captureHistory = filtered
	if len(w.captureHistory) >= w.cfg.Capture.MaxCapturesPerHour {
		return false
	}
	w.captureHistory = append(w.captureHistory, now)
	w.lastCaptureAt = now
	return true
}

func (w *Watcher) currentRuntime() *ResolvedRuntime {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runtime == nil {
		return nil
	}
	clone := *w.runtime
	return &clone
}

func (w *Watcher) capture(ctx context.Context, sample CPUSample, trigger string) error {
	if err := w.refreshRuntime(true); err != nil {
		w.logRateLimited("runtime_refresh_capture", time.Minute, log.WarnLevel, "failed to refresh runtime before capture: %v", err)
	}
	runtimeInfo := w.currentRuntime()
	now := time.Now()
	captureDir := filepath.Join(w.cfg.Capture.OutputDir, fmt.Sprintf("%s-pid%d-%s-%.1f", now.Format("20060102-150405"), sample.Process.PID, trigger, sample.CPUPercent))
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		return fmt.Errorf("create capture dir: %w", err)
	}

	metadata := captureMetadata{
		CapturedAt:         now,
		Trigger:            trigger,
		ObservedCPUPercent: sample.CPUPercent,
		SamplingInterval:   w.cfg.Sampling.Interval.String(),
		PID:                sample.Process.PID,
		Comm:               sample.Process.Comm,
		Cmdline:            sample.Process.Cmdline,
		Runtime:            runtimeInfo,
		CaptureOutputDir:   captureDir,
		CPUProfileSeconds:  w.cfg.Capture.CPUProfileSeconds,
		Cooldown:           w.cfg.Capture.Cooldown.String(),
		MaxCapturesPerHour: w.cfg.Capture.MaxCapturesPerHour,
		SustainedThreshold: w.cfg.Sampling.SustainedThresholdPercent,
		BurstThreshold:     w.cfg.Sampling.BurstThresholdPercent,
		SustainedSamples:   w.cfg.Sampling.SustainedConsecutiveSamples,
	}

	var captureErrors []string
	addError := func(format string, args ...any) {
		errMsg := fmt.Sprintf(format, args...)
		captureErrors = append(captureErrors, errMsg)
		log.Warn(errMsg)
	}

	if err := writeTextFile(filepath.Join(captureDir, "process.txt"), fmt.Sprintf("pid: %d\ncomm: %s\ncmdline: %s\ncpu_percent: %.2f\ntrigger: %s\n", sample.Process.PID, sample.Process.Comm, sample.Process.Cmdline, sample.CPUPercent, trigger)); err != nil {
		addError("write process.txt: %v", err)
	}
	if err := w.writeProcSnapshot(captureDir, sample.Process.PID); err != nil {
		addError("write proc snapshot: %v", err)
	}
	if runtimeInfo != nil && runtimeInfo.CLIProxyConfigPath != "" {
		if err := copyFile(runtimeInfo.CLIProxyConfigPath, filepath.Join(captureDir, "cliproxyapi.yaml")); err != nil {
			addError("copy cliproxy config snapshot: %v", err)
		}
	}
	if err := w.writeCommandSnapshot(ctx, captureDir, sample.Process.PID, runtimeInfo); err != nil {
		addError("write command snapshots: %v", err)
	}
	if runtimeInfo != nil && runtimeInfo.RequestLogPath != "" && w.cfg.Capture.LogTailLines > 0 {
		if tail, err := tailFileLines(runtimeInfo.RequestLogPath, w.cfg.Capture.LogTailLines); err != nil {
			addError("tail request log %s: %v", runtimeInfo.RequestLogPath, err)
		} else if err := writeTextFile(filepath.Join(captureDir, "request-log-tail.txt"), tail); err != nil {
			addError("write request-log-tail.txt: %v", err)
		}
	}
	if runtimeInfo != nil && runtimeInfo.PprofBaseURL != "" {
		if err := w.writePprofCapture(ctx, captureDir, runtimeInfo); err != nil {
			addError("write pprof capture: %v", err)
		}
	} else {
		addError("pprof base url is empty, skipping pprof capture")
	}

	metadata.Errors = captureErrors
	if err := writeYAMLFile(filepath.Join(captureDir, "metadata.yaml"), metadata); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	if err := w.applyRetention(); err != nil {
		addError("apply retention: %v", err)
		metadata.Errors = captureErrors
		_ = writeYAMLFile(filepath.Join(captureDir, "metadata.yaml"), metadata)
	}
	log.WithFields(log.Fields{"dir": captureDir, "trigger": trigger, "cpu": fmt.Sprintf("%.2f", sample.CPUPercent)}).Info("captured cliproxy profiler evidence")
	return nil
}

func (w *Watcher) writePprofCapture(ctx context.Context, captureDir string, runtimeInfo *ResolvedRuntime) error {
	if runtimeInfo == nil || runtimeInfo.PprofBaseURL == "" {
		return fmt.Errorf("pprof runtime is not available")
	}

	cpuURL := fmt.Sprintf("%s/profile?seconds=%d", runtimeInfo.PprofBaseURL, w.cfg.Capture.CPUProfileSeconds)
	if err := w.downloadToFile(ctx, cpuURL, filepath.Join(captureDir, "cpu.pb.gz"), w.cfg.Capture.RequestTimeout.Duration+time.Duration(w.cfg.Capture.CPUProfileSeconds)*time.Second+5*time.Second); err != nil {
		return fmt.Errorf("cpu profile: %w", err)
	}
	if err := w.downloadToFile(ctx, runtimeInfo.PprofBaseURL+"/cmdline", filepath.Join(captureDir, "cmdline.txt"), w.cfg.Capture.RequestTimeout.Duration); err != nil {
		log.WithError(err).Warn("failed to capture pprof cmdline")
	}
	if w.cfg.Capture.IncludeHeap {
		if err := w.downloadToFile(ctx, runtimeInfo.PprofBaseURL+"/heap", filepath.Join(captureDir, "heap.pb.gz"), w.cfg.Capture.RequestTimeout.Duration); err != nil {
			log.WithError(err).Warn("failed to capture heap profile")
		}
	}
	if w.cfg.Capture.IncludeThreadcreate {
		if err := w.downloadToFile(ctx, runtimeInfo.PprofBaseURL+"/threadcreate", filepath.Join(captureDir, "threadcreate.pb.gz"), w.cfg.Capture.RequestTimeout.Duration); err != nil {
			log.WithError(err).Warn("failed to capture threadcreate profile")
		}
	}
	if w.cfg.Capture.IncludeGoroutine {
		if err := w.downloadToFile(ctx, runtimeInfo.PprofBaseURL+"/goroutine?debug=2", filepath.Join(captureDir, "goroutine.txt"), w.cfg.Capture.RequestTimeout.Duration); err != nil {
			log.WithError(err).Warn("failed to capture goroutine dump")
		}
	}
	return nil
}

func (w *Watcher) downloadToFile(ctx context.Context, sourceURL, destPath string, timeout time.Duration) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close pprof response body")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	file, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close capture file")
		}
	}()
	if _, err := io.Copy(file, resp.Body); err != nil {
		return err
	}
	return nil
}

func (w *Watcher) writeProcSnapshot(captureDir string, pid int) error {
	files := map[string]string{
		"proc-status.txt": filepath.Join("/proc", strconv.Itoa(pid), "status"),
		"proc-limits.txt": filepath.Join("/proc", strconv.Itoa(pid), "limits"),
		"proc-sched.txt":  filepath.Join("/proc", strconv.Itoa(pid), "sched"),
		"proc-io.txt":     filepath.Join("/proc", strconv.Itoa(pid), "io"),
	}
	for name, source := range files {
		if err := copyFile(source, filepath.Join(captureDir, name)); err != nil {
			log.WithError(err).Warnf("failed to snapshot %s", source)
		}
	}
	return nil
}

func (w *Watcher) writeCommandSnapshot(ctx context.Context, captureDir string, pid int, runtimeInfo *ResolvedRuntime) error {
	commands := []struct {
		name string
		args []string
	}{
		{name: "ps-process.txt", args: []string{"ps", "-p", strconv.Itoa(pid), "-o", "pid,ppid,etimes,pcpu,pmem,state,cmd"}},
		{name: "ps-threads.txt", args: []string{"ps", "-Tp", strconv.Itoa(pid), "-o", "pid,tid,pcpu,pmem,state,time,comm", "--sort=-pcpu"}},
		{name: "ss-summary.txt", args: []string{"ss", "-s"}},
	}
	for _, cmdSpec := range commands {
		output, err := runCommand(ctx, 5*time.Second, cmdSpec.args[0], cmdSpec.args[1:]...)
		if err != nil {
			log.WithError(err).Warnf("failed to run %s", strings.Join(cmdSpec.args, " "))
			continue
		}
		if err := writeTextFile(filepath.Join(captureDir, cmdSpec.name), output); err != nil {
			log.WithError(err).Warnf("failed to write %s", cmdSpec.name)
		}
	}
	if w.cfg.Capture.IncludeSocketDetails {
		output, err := runCommand(ctx, 5*time.Second, "ss", "-tanp")
		if err != nil {
			return err
		}
		filtered := filterSocketDetails(output, runtimeInfo)
		if err := writeTextFile(filepath.Join(captureDir, "ss-details.txt"), filtered); err != nil {
			return err
		}
	}
	return nil
}

func filterSocketDetails(output string, runtimeInfo *ResolvedRuntime) string {
	if runtimeInfo == nil {
		return output
	}
	ports := make([]string, 0, 2)
	if runtimeInfo.ServicePort > 0 {
		ports = append(ports, fmt.Sprintf(":%d", runtimeInfo.ServicePort))
	}
	if runtimeInfo.PprofPort != "" {
		ports = append(ports, ":"+runtimeInfo.PprofPort)
	}
	if len(ports) == 0 {
		return output
	}

	var builder strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(output))
	firstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if firstLine {
			builder.WriteString(line)
			builder.WriteByte('\n')
			firstLine = false
			continue
		}
		for _, port := range ports {
			if strings.Contains(line, port) {
				builder.WriteString(line)
				builder.WriteByte('\n')
				break
			}
		}
	}
	if builder.Len() == 0 {
		return output
	}
	return builder.String()
}

func runCommand(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return stdout.String(), fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), fmt.Errorf("run %s: %w", name, err)
	}
	if stderr.Len() > 0 {
		stdout.WriteByte('\n')
		stdout.WriteString("# stderr\n")
		stdout.Write(stderr.Bytes())
	}
	return stdout.String(), nil
}

func writeTextFile(path string, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeYAMLFile(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func copyFile(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := in.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close input file")
		}
	}()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := out.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close output file")
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

func tailFileLines(path string, lineCount int) (string, error) {
	if lineCount <= 0 {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close tailed file")
		}
	}()
	stat, err := file.Stat()
	if err != nil {
		return "", err
	}
	if stat.Size() == 0 {
		return "", nil
	}

	const chunkSize int64 = 4096
	size := stat.Size()
	var offset int64 = size
	buffer := make([]byte, 0, minInt64(size, 64*1024))
	linesFound := 0
	for offset > 0 && linesFound <= lineCount {
		readSize := minInt64(chunkSize, offset)
		offset -= readSize
		chunk := make([]byte, readSize)
		if _, err := file.ReadAt(chunk, offset); err != nil {
			return "", err
		}
		buffer = append(chunk, buffer...)
		linesFound = bytes.Count(buffer, []byte{'\n'})
	}
	lines := strings.Split(strings.TrimRight(string(buffer), "\n"), "\n")
	if len(lines) > lineCount {
		lines = lines[len(lines)-lineCount:]
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (w *Watcher) applyRetention() error {
	entries, err := os.ReadDir(w.cfg.Capture.OutputDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	type captureDirInfo struct {
		path    string
		modTime time.Time
		size    int64
	}

	captures := make([]captureDirInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(w.cfg.Capture.OutputDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		size, err := dirSize(fullPath)
		if err != nil {
			continue
		}
		captures = append(captures, captureDirInfo{path: fullPath, modTime: info.ModTime(), size: size})
	}
	if len(captures) == 0 {
		return nil
	}

	sort.Slice(captures, func(i, j int) bool {
		return captures[i].modTime.Before(captures[j].modTime)
	})

	if w.cfg.Retention.MaxAge.Duration > 0 {
		cutoff := time.Now().Add(-w.cfg.Retention.MaxAge.Duration)
		for len(captures) > 0 && captures[0].modTime.Before(cutoff) {
			if err := os.RemoveAll(captures[0].path); err != nil {
				return fmt.Errorf("remove expired capture %s: %w", captures[0].path, err)
			}
			captures = captures[1:]
		}
	}

	for w.cfg.Retention.MaxCaptureDirs > 0 && len(captures) > w.cfg.Retention.MaxCaptureDirs {
		if err := os.RemoveAll(captures[0].path); err != nil {
			return fmt.Errorf("remove old capture %s: %w", captures[0].path, err)
		}
		captures = captures[1:]
	}

	if w.cfg.Retention.MaxTotalSizeMB > 0 {
		var totalBytes int64
		for _, capture := range captures {
			totalBytes += capture.size
		}
		limitBytes := w.cfg.Retention.MaxTotalSizeMB * 1024 * 1024
		for totalBytes > limitBytes && len(captures) > 0 {
			if err := os.RemoveAll(captures[0].path); err != nil {
				return fmt.Errorf("remove capture for total size limit %s: %w", captures[0].path, err)
			}
			totalBytes -= captures[0].size
			captures = captures[1:]
		}
	}

	return nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func (w *Watcher) logRateLimited(key string, interval time.Duration, level log.Level, format string, args ...any) {
	w.mu.Lock()
	last := w.lastLogByKey[key]
	if time.Since(last) < interval {
		w.mu.Unlock()
		return
	}
	w.lastLogByKey[key] = time.Now()
	w.mu.Unlock()

	message := fmt.Sprintf(format, args...)
	switch level {
	case log.DebugLevel:
		log.Debug(message)
	case log.InfoLevel:
		log.Info(message)
	case log.WarnLevel:
		log.Warn(message)
	default:
		log.Print(message)
	}
}
