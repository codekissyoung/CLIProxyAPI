package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigBytes_LogsMaxTotalSizeDefaultAndExplicitZero(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`port: 8317`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.LogsMaxTotalSizeMB != DefaultLogsMaxTotalSizeMB {
		t.Fatalf("LogsMaxTotalSizeMB = %d, want %d", cfg.LogsMaxTotalSizeMB, DefaultLogsMaxTotalSizeMB)
	}

	cfg, errParse = ParseConfigBytes([]byte(`
port: 8317
logs-max-total-size-mb: 0
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.LogsMaxTotalSizeMB != 0 {
		t.Fatalf("LogsMaxTotalSizeMB = %d, want 0", cfg.LogsMaxTotalSizeMB)
	}
}

func TestLoadConfigOptional_LogsMaxTotalSizeDefaultAndExplicitZero(t *testing.T) {
	dir := t.TempDir()

	defaultPath := filepath.Join(dir, "default.yaml")
	if errWrite := os.WriteFile(defaultPath, []byte(`port: 8317`), 0o644); errWrite != nil {
		t.Fatalf("write default config: %v", errWrite)
	}
	cfg, errLoad := LoadConfigOptional(defaultPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if cfg.LogsMaxTotalSizeMB != DefaultLogsMaxTotalSizeMB {
		t.Fatalf("LogsMaxTotalSizeMB = %d, want %d", cfg.LogsMaxTotalSizeMB, DefaultLogsMaxTotalSizeMB)
	}

	explicitZeroPath := filepath.Join(dir, "explicit-zero.yaml")
	if errWrite := os.WriteFile(explicitZeroPath, []byte(`
port: 8317
logs-max-total-size-mb: 0
`), 0o644); errWrite != nil {
		t.Fatalf("write explicit-zero config: %v", errWrite)
	}
	cfg, errLoad = LoadConfigOptional(explicitZeroPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if cfg.LogsMaxTotalSizeMB != 0 {
		t.Fatalf("LogsMaxTotalSizeMB = %d, want 0", cfg.LogsMaxTotalSizeMB)
	}
}
