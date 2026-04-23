package profilerwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ProcessMatcher identifies the target process.
type ProcessMatcher struct {
	PID              int
	Comm             string
	CommandSubstring string
}

// ProcessInfo describes the matched process.
type ProcessInfo struct {
	PID     int
	Comm    string
	Cmdline string
	Stat    ProcStat
}

// ProcStat contains the subset of /proc/<pid>/stat fields required by the watcher.
type ProcStat struct {
	UTime     uint64
	STime     uint64
	StartTime uint64
}

// TotalCPUTime returns the accumulated user+system process time.
func (s ProcStat) TotalCPUTime() uint64 {
	return s.UTime + s.STime
}

// CPUSample captures a single watcher sample.
type CPUSample struct {
	CPUPercent float64
	Process    ProcessInfo
}

func readProcessInfo(pid int) (*ProcessInfo, error) {
	commBytes, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return nil, fmt.Errorf("read comm for pid %d: %w", pid, err)
	}
	cmdlineBytes, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, fmt.Errorf("read cmdline for pid %d: %w", pid, err)
	}
	stat, err := readProcStat(pid)
	if err != nil {
		return nil, err
	}

	cmdline := strings.TrimSpace(strings.ReplaceAll(string(cmdlineBytes), "\x00", " "))
	return &ProcessInfo{
		PID:     pid,
		Comm:    strings.TrimSpace(string(commBytes)),
		Cmdline: cmdline,
		Stat:    stat,
	}, nil
}

// FindProcess locates the newest process matching the configured constraints.
func FindProcess(matcher ProcessMatcher) (*ProcessInfo, error) {
	if matcher.PID > 0 {
		info, err := readProcessInfo(matcher.PID)
		if err != nil {
			return nil, err
		}
		if !matchesProcess(*info, matcher) {
			return nil, fmt.Errorf("pid %d does not match configured process filters", matcher.PID)
		}
		return info, nil
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}

	var best *ProcessInfo
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := readProcessInfo(pid)
		if err != nil {
			continue
		}
		if !matchesProcess(*info, matcher) {
			continue
		}
		if best == nil || info.Stat.StartTime > best.Stat.StartTime || (info.Stat.StartTime == best.Stat.StartTime && info.PID > best.PID) {
			best = info
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no matching process found")
	}
	return best, nil
}

func matchesProcess(info ProcessInfo, matcher ProcessMatcher) bool {
	if matcher.Comm != "" && info.Comm != matcher.Comm {
		return false
	}
	if matcher.CommandSubstring != "" && !strings.Contains(info.Cmdline, matcher.CommandSubstring) {
		return false
	}
	return true
}

func readProcStat(pid int) (ProcStat, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcStat{}, fmt.Errorf("read stat for pid %d: %w", pid, err)
	}
	return parseProcStat(string(data))
}

func parseProcStat(raw string) (ProcStat, error) {
	raw = strings.TrimSpace(raw)
	closeIdx := strings.LastIndex(raw, ")")
	if closeIdx == -1 || closeIdx+2 >= len(raw) {
		return ProcStat{}, fmt.Errorf("unexpected /proc stat format")
	}
	fields := strings.Fields(raw[closeIdx+2:])
	if len(fields) < 20 {
		return ProcStat{}, fmt.Errorf("unexpected /proc stat field count: %d", len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return ProcStat{}, fmt.Errorf("parse utime: %w", err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return ProcStat{}, fmt.Errorf("parse stime: %w", err)
	}
	starttime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return ProcStat{}, fmt.Errorf("parse starttime: %w", err)
	}
	return ProcStat{UTime: utime, STime: stime, StartTime: starttime}, nil
}

func readTotalCPUTime() (uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		var total uint64
		for _, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse /proc/stat field %q: %w", field, err)
			}
			total += value
		}
		return total, nil
	}
	return 0, fmt.Errorf("cpu line not found in /proc/stat")
}

// CPUSampler derives process CPU percentages from /proc snapshots.
type CPUSampler struct {
	initialized bool
	pid         int
	lastTotal   uint64
	lastProc    uint64
	numCPU      float64
}

// NewCPUSampler constructs a sampler using the current logical CPU count.
func NewCPUSampler() *CPUSampler {
	return &CPUSampler{numCPU: float64(runtime.NumCPU())}
}

// Sample returns the current CPU percentage or ok=false during sampler warm-up.
func (s *CPUSampler) Sample(process ProcessInfo) (*CPUSample, bool, error) {
	total, err := readTotalCPUTime()
	if err != nil {
		return nil, false, err
	}
	proc := process.Stat.TotalCPUTime()
	if !s.initialized || s.pid != process.PID || total <= s.lastTotal || proc < s.lastProc {
		s.pid = process.PID
		s.lastTotal = total
		s.lastProc = proc
		s.initialized = true
		return nil, false, nil
	}

	deltaTotal := total - s.lastTotal
	deltaProc := proc - s.lastProc
	s.lastTotal = total
	s.lastProc = proc
	if deltaTotal == 0 {
		return nil, false, fmt.Errorf("delta total cpu time is zero")
	}

	cpuPercent := (float64(deltaProc) / float64(deltaTotal)) * s.numCPU * 100
	return &CPUSample{
		CPUPercent: cpuPercent,
		Process:    process,
	}, true, nil
}
