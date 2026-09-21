package config

import "testing"

func TestTurnStateInjectionMode(t *testing.T) {
	var nilCfg *CodexConfig
	if enabled, dryRun := nilCfg.TurnStateInjectionMode(); enabled || dryRun {
		t.Fatalf("nil config: enabled=%v dryRun=%v, want disabled", enabled, dryRun)
	}

	cases := []struct {
		value       string
		wantEnabled bool
		wantDryRun  bool
	}{
		{"", false, false},
		{"off", false, false},
		{"bogus", false, false},
		{"dry-run", true, true},
		{"Dry-Run", true, true},
		{"  dry-run  ", true, true},
		{"enforce", true, false},
		{"ENFORCE", true, false},
	}
	for _, tc := range cases {
		cfg := &CodexConfig{TurnStateInject: tc.value}
		enabled, dryRun := cfg.TurnStateInjectionMode()
		if enabled != tc.wantEnabled || dryRun != tc.wantDryRun {
			t.Errorf("TurnStateInject=%q: enabled=%v dryRun=%v, want enabled=%v dryRun=%v",
				tc.value, enabled, dryRun, tc.wantEnabled, tc.wantDryRun)
		}
	}
}

func TestParseConfigBytesTurnStateInject(t *testing.T) {
	raw := []byte("codex:\n  turn-state-capture: true\n  turn-state-inject: dry-run\n")
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if !cfg.Codex.TurnStateCapture {
		t.Fatal("turn-state-capture should be true")
	}
	if cfg.Codex.TurnStateInject != "dry-run" {
		t.Fatalf("turn-state-inject = %q, want dry-run", cfg.Codex.TurnStateInject)
	}
}
