package profilerwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML config can use human-readable values such as 1s or 3m.
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses duration strings from YAML.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar, got kind %d", node.Kind)
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML renders the duration as a string for metadata and config output.
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

// String returns a stable textual representation.
func (d Duration) String() string {
	if d.Duration == 0 {
		return "0s"
	}
	return d.Duration.String()
}

// Config defines the cliproxy profiler watcher settings.
type Config struct {
	Target    TargetConfig    `yaml:"target"`
	Sampling  SamplingConfig  `yaml:"sampling"`
	Capture   CaptureConfig   `yaml:"capture"`
	Retention RetentionConfig `yaml:"retention"`
	LogLevel  string          `yaml:"log-level"`
}

// TargetConfig defines which process and pprof endpoint should be observed.
type TargetConfig struct {
	PID                int    `yaml:"pid"`
	Comm               string `yaml:"comm"`
	CommandSubstring   string `yaml:"command-substring"`
	CLIProxyConfigPath string `yaml:"cliproxy-config-path"`
	PprofURL           string `yaml:"pprof-url"`
	RequestLogPath     string `yaml:"request-log-path"`
}

// SamplingConfig controls CPU polling and trigger thresholds.
type SamplingConfig struct {
	Interval                    Duration `yaml:"interval"`
	BurstThresholdPercent       float64  `yaml:"burst-threshold-percent"`
	SustainedThresholdPercent   float64  `yaml:"sustained-threshold-percent"`
	SustainedConsecutiveSamples int      `yaml:"sustained-consecutive-samples"`
}

// CaptureConfig controls what evidence is captured when a threshold is hit.
type CaptureConfig struct {
	OutputDir            string   `yaml:"output-dir"`
	Cooldown             Duration `yaml:"cooldown"`
	MaxCapturesPerHour   int      `yaml:"max-captures-per-hour"`
	CPUProfileSeconds    int      `yaml:"cpu-profile-seconds"`
	RequestTimeout       Duration `yaml:"request-timeout"`
	LogTailLines         int      `yaml:"log-tail-lines"`
	IncludeHeap          bool     `yaml:"include-heap"`
	IncludeGoroutine     bool     `yaml:"include-goroutine"`
	IncludeThreadcreate  bool     `yaml:"include-threadcreate"`
	IncludeSocketDetails bool     `yaml:"include-socket-details"`
}

// RetentionConfig limits how much evidence remains on disk.
type RetentionConfig struct {
	MaxCaptureDirs int      `yaml:"max-capture-dirs"`
	MaxTotalSizeMB int64    `yaml:"max-total-size-mb"`
	MaxAge         Duration `yaml:"max-age"`
}

// LoadConfig reads and validates a profiler watcher config file.
func LoadConfig(path string) (*Config, error) {
	expandedPath, err := expandPath(path)
	if err != nil {
		return nil, fmt.Errorf("expand config path: %w", err)
	}

	data, err := os.ReadFile(expandedPath)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaultConfig() Config {
	return Config{
		Target: TargetConfig{
			Comm: "cliproxyapi",
		},
		Sampling: SamplingConfig{
			Interval:                    Duration{Duration: time.Second},
			BurstThresholdPercent:       70,
			SustainedThresholdPercent:   40,
			SustainedConsecutiveSamples: 2,
		},
		Capture: CaptureConfig{
			OutputDir:            "./cliproxy-profiler-captures",
			Cooldown:             Duration{Duration: 3 * time.Minute},
			MaxCapturesPerHour:   3,
			CPUProfileSeconds:    10,
			RequestTimeout:       Duration{Duration: 10 * time.Second},
			LogTailLines:         120,
			IncludeHeap:          true,
			IncludeGoroutine:     true,
			IncludeThreadcreate:  true,
			IncludeSocketDetails: false,
		},
		Retention: RetentionConfig{
			MaxCaptureDirs: 50,
			MaxTotalSizeMB: 2048,
			MaxAge:         Duration{Duration: 7 * 24 * time.Hour},
		},
		LogLevel: "info",
	}
}

func (c *Config) normalize() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}

	var err error
	c.Target.CLIProxyConfigPath, err = expandPath(c.Target.CLIProxyConfigPath)
	if err != nil {
		return fmt.Errorf("expand target.cliproxy-config-path: %w", err)
	}
	c.Target.RequestLogPath, err = expandPath(c.Target.RequestLogPath)
	if err != nil {
		return fmt.Errorf("expand target.request-log-path: %w", err)
	}
	c.Capture.OutputDir, err = expandPath(c.Capture.OutputDir)
	if err != nil {
		return fmt.Errorf("expand capture.output-dir: %w", err)
	}

	c.Target.Comm = strings.TrimSpace(c.Target.Comm)
	c.Target.CommandSubstring = strings.TrimSpace(c.Target.CommandSubstring)
	c.Target.PprofURL = strings.TrimSpace(c.Target.PprofURL)
	c.Target.CLIProxyConfigPath = strings.TrimSpace(c.Target.CLIProxyConfigPath)
	c.Target.RequestLogPath = strings.TrimSpace(c.Target.RequestLogPath)
	c.Capture.OutputDir = strings.TrimSpace(c.Capture.OutputDir)
	c.LogLevel = strings.TrimSpace(strings.ToLower(c.LogLevel))

	if c.Target.PID < 0 {
		return fmt.Errorf("target.pid must be >= 0")
	}
	if c.Target.PID == 0 && c.Target.Comm == "" && c.Target.CommandSubstring == "" {
		return fmt.Errorf("target must define pid or at least one process matcher")
	}
	if c.Target.PprofURL == "" && c.Target.CLIProxyConfigPath == "" {
		return fmt.Errorf("either target.pprof-url or target.cliproxy-config-path must be set")
	}
	if c.Sampling.Interval.Duration <= 0 {
		return fmt.Errorf("sampling.interval must be > 0")
	}
	if c.Sampling.BurstThresholdPercent < 0 {
		return fmt.Errorf("sampling.burst-threshold-percent must be >= 0")
	}
	if c.Sampling.SustainedThresholdPercent < 0 {
		return fmt.Errorf("sampling.sustained-threshold-percent must be >= 0")
	}
	if c.Sampling.SustainedConsecutiveSamples <= 0 {
		return fmt.Errorf("sampling.sustained-consecutive-samples must be > 0")
	}
	if c.Capture.OutputDir == "" {
		return fmt.Errorf("capture.output-dir must not be empty")
	}
	if c.Capture.Cooldown.Duration < 0 {
		return fmt.Errorf("capture.cooldown must be >= 0")
	}
	if c.Capture.MaxCapturesPerHour <= 0 {
		return fmt.Errorf("capture.max-captures-per-hour must be > 0")
	}
	if c.Capture.CPUProfileSeconds <= 0 {
		return fmt.Errorf("capture.cpu-profile-seconds must be > 0")
	}
	if c.Capture.RequestTimeout.Duration <= 0 {
		return fmt.Errorf("capture.request-timeout must be > 0")
	}
	if c.Capture.LogTailLines < 0 {
		return fmt.Errorf("capture.log-tail-lines must be >= 0")
	}
	if c.Retention.MaxCaptureDirs < 0 {
		return fmt.Errorf("retention.max-capture-dirs must be >= 0")
	}
	if c.Retention.MaxTotalSizeMB < 0 {
		return fmt.Errorf("retention.max-total-size-mb must be >= 0")
	}
	if c.Retention.MaxAge.Duration < 0 {
		return fmt.Errorf("retention.max-age must be >= 0")
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	return nil
}

func expandPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		remainder := strings.TrimPrefix(trimmed, "~")
		remainder = strings.TrimLeft(remainder, "/\\")
		if remainder == "" {
			return filepath.Clean(home), nil
		}
		trimmed = filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(remainder, "\\", "/")))
	}
	return filepath.Clean(trimmed), nil
}
