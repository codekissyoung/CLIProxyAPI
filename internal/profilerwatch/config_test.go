package profilerwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaultsAndOverrides(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "cliproxy-profiler.yaml")
	data := []byte(`target:
  cliproxy-config-path: /home/iec/deploy/etc/cliproxyapi.yaml
sampling:
  interval: 2s
capture:
  output-dir: ~/captures
  cpu-profile-seconds: 15
`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Target.Comm != "cliproxyapi" {
		t.Fatalf("expected default comm cliproxyapi, got %q", cfg.Target.Comm)
	}
	if got := cfg.Sampling.Interval.Duration; got != 2*time.Second {
		t.Fatalf("expected interval 2s, got %v", got)
	}
	if got := cfg.Capture.CPUProfileSeconds; got != 15 {
		t.Fatalf("expected cpu profile seconds 15, got %d", got)
	}
	if got := cfg.Capture.Cooldown.Duration; got != 3*time.Minute {
		t.Fatalf("expected default cooldown 3m, got %v", got)
	}
	if got := cfg.Retention.MaxCaptureDirs; got != 50 {
		t.Fatalf("expected default max capture dirs 50, got %d", got)
	}
	if cfg.Capture.OutputDir == "~/captures" {
		t.Fatalf("expected output dir to expand ~, got %q", cfg.Capture.OutputDir)
	}
}

func TestNormalizePprofBaseURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "host_port", raw: "127.0.0.1:8316", want: "http://127.0.0.1:8316/debug/pprof"},
		{name: "base_path", raw: "http://127.0.0.1:8316", want: "http://127.0.0.1:8316/debug/pprof"},
		{name: "already_pprof", raw: "http://127.0.0.1:8316/debug/pprof/", want: "http://127.0.0.1:8316/debug/pprof"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePprofBaseURL(tt.raw)
			if err != nil {
				t.Fatalf("normalizePprofBaseURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizePprofBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseProcStat(t *testing.T) {
	raw := `12345 (cliproxyapi) S 1 2 3 4 5 6 7 8 9 10 100 200 13 14 15 16 17 18 300 21 22 23`
	stat, err := parseProcStat(raw)
	if err != nil {
		t.Fatalf("parseProcStat() error = %v", err)
	}
	if stat.UTime != 100 {
		t.Fatalf("expected utime 100, got %d", stat.UTime)
	}
	if stat.STime != 200 {
		t.Fatalf("expected stime 200, got %d", stat.STime)
	}
	if stat.StartTime != 300 {
		t.Fatalf("expected starttime 300, got %d", stat.StartTime)
	}
}
