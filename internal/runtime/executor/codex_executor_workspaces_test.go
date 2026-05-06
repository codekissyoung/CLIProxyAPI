package executor

import (
	"net/http"
	"strings"
	"testing"
)

func TestStripCodexTurnMetadataWorkspaces_RemovesWorkspacesField(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Turn-Metadata", `{"session_id":"sess-1","turn_id":"turn-1","sandbox":"none","workspaces":{"/Users/link/repo":{"associated_remote_urls":{"origin":"git@github.com:codekissyoung/private.git"},"latest_git_commit_hash":"abc","has_changes":false}}}`)

	stripCodexTurnMetadataWorkspaces(h)

	got := h.Get("X-Codex-Turn-Metadata")
	if got == "" {
		t.Fatalf("metadata header was unexpectedly cleared: %v", h)
	}
	if strings.Contains(got, "workspaces") || strings.Contains(got, "codekissyoung") || strings.Contains(got, "/Users/link") {
		t.Fatalf("workspaces leak survived strip: %s", got)
	}
	if !strings.Contains(got, `"session_id":"sess-1"`) || !strings.Contains(got, `"turn_id":"turn-1"`) {
		t.Fatalf("benign fields were dropped: %s", got)
	}
}

func TestStripCodexTurnMetadataWorkspaces_NoOpWhenAbsent(t *testing.T) {
	h := http.Header{}
	original := `{"session_id":"sess-1","turn_id":"turn-1"}`
	h.Set("X-Codex-Turn-Metadata", original)

	stripCodexTurnMetadataWorkspaces(h)

	if got := h.Get("X-Codex-Turn-Metadata"); got != original {
		t.Fatalf("header mutated unnecessarily: got %s, want %s", got, original)
	}
}

func TestStripCodexTurnMetadataWorkspaces_NoOpOnNonJSON(t *testing.T) {
	h := http.Header{}
	original := "not-json-at-all"
	h.Set("X-Codex-Turn-Metadata", original)

	stripCodexTurnMetadataWorkspaces(h)

	if got := h.Get("X-Codex-Turn-Metadata"); got != original {
		t.Fatalf("non-JSON header mutated: got %s, want %s", got, original)
	}
}

func TestStripCodexTurnMetadataWorkspaces_PreservesLowercaseKey(t *testing.T) {
	h := http.Header{
		"x-codex-turn-metadata": []string{`{"session_id":"sess-1","workspaces":{"/p":{}}}`},
	}

	stripCodexTurnMetadataWorkspaces(h)

	if _, ok := h["x-codex-turn-metadata"]; !ok {
		t.Fatalf("original lowercase key was dropped: %#v", h)
	}
	if _, ok := h["X-Codex-Turn-Metadata"]; ok {
		t.Fatalf("strip introduced canonical key alongside lowercase: %#v", h)
	}
	if got := h["x-codex-turn-metadata"][0]; strings.Contains(got, "workspaces") {
		t.Fatalf("workspaces survived: %s", got)
	}
}

func TestStripCodexTurnMetadataWorkspaces_NoHeaderNoOp(t *testing.T) {
	h := http.Header{}
	stripCodexTurnMetadataWorkspaces(h)
	if len(h) != 0 {
		t.Fatalf("strip created header out of nothing: %#v", h)
	}
	stripCodexTurnMetadataWorkspaces(nil)
}
