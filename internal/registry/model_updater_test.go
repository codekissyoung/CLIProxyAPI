package registry

import (
	"strings"
	"testing"
)

func TestValidateModelsCatalog_AllowsEmptyOptionalSections(t *testing.T) {
	data := validTestModelsCatalog()
	data.Qwen = nil
	data.IFlow = nil

	if err := validateModelsCatalog(data); err != nil {
		t.Fatalf("validateModelsCatalog() error = %v, want nil", err)
	}
}

func TestValidateModelsCatalog_StillRequiresCodexSections(t *testing.T) {
	data := validTestModelsCatalog()
	data.CodexPro = nil

	err := validateModelsCatalog(data)
	if err == nil {
		t.Fatal("validateModelsCatalog() error = nil, want codex-pro validation failure")
	}
	if !strings.Contains(err.Error(), "codex-pro section is empty") {
		t.Fatalf("validateModelsCatalog() error = %v, want codex-pro section is empty", err)
	}
}

func TestValidateModelsCatalog_StillValidatesNonEmptyQwenSection(t *testing.T) {
	data := validTestModelsCatalog()
	data.Qwen = []*ModelInfo{{ID: ""}}

	err := validateModelsCatalog(data)
	if err == nil {
		t.Fatal("validateModelsCatalog() error = nil, want qwen validation failure")
	}
	if !strings.Contains(err.Error(), "qwen[0] has empty id") {
		t.Fatalf("validateModelsCatalog() error = %v, want qwen[0] has empty id", err)
	}
}

func TestValidateModelsCatalog_StillValidatesNonEmptyIFlowSection(t *testing.T) {
	data := validTestModelsCatalog()
	data.IFlow = []*ModelInfo{{ID: ""}}

	err := validateModelsCatalog(data)
	if err == nil {
		t.Fatal("validateModelsCatalog() error = nil, want iflow validation failure")
	}
	if !strings.Contains(err.Error(), "iflow[0] has empty id") {
		t.Fatalf("validateModelsCatalog() error = %v, want iflow[0] has empty id", err)
	}
}

func validTestModelsCatalog() *staticModelsJSON {
	return &staticModelsJSON{
		Claude:      []*ModelInfo{{ID: "claude-test"}},
		Gemini:      []*ModelInfo{{ID: "gemini-test"}},
		Vertex:      []*ModelInfo{{ID: "vertex-test"}},
		GeminiCLI:   []*ModelInfo{{ID: "gemini-cli-test"}},
		AIStudio:    []*ModelInfo{{ID: "aistudio-test"}},
		CodexFree:   []*ModelInfo{{ID: "codex-free-test"}},
		CodexTeam:   []*ModelInfo{{ID: "codex-team-test"}},
		CodexPlus:   []*ModelInfo{{ID: "codex-plus-test"}},
		CodexPro:    []*ModelInfo{{ID: "codex-pro-test"}},
		Qwen:        []*ModelInfo{{ID: "qwen-test"}},
		IFlow:       []*ModelInfo{{ID: "iflow-test"}},
		Kimi:        []*ModelInfo{{ID: "kimi-test"}},
		Antigravity: []*ModelInfo{{ID: "antigravity-test"}},
	}
}
