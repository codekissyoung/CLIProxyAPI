package executor

import (
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// The forced-UA hardening pins a real macOS Codex Desktop UA per pool account.
// These tests lock the pool's integrity and the per-account selection so the
// anti-detection guarantees (real build, stable per account, spread across
// accounts, Originator in lockstep) can't silently regress.

func TestCodexFallbackUserAgentPoolEntriesAreConsistentMacOSCodexDesktop(t *testing.T) {
	if codexFallbackUserAgentPool[0] != codexUserAgent {
		t.Fatalf("pool[0] = %q, must equal codexUserAgent (the nil/empty-ID fallback)", codexFallbackUserAgentPool[0])
	}
	seen := make(map[string]struct{}, len(codexFallbackUserAgentPool))
	for i, ua := range codexFallbackUserAgentPool {
		if _, dup := seen[ua]; dup {
			t.Fatalf("pool[%d] = %q is a duplicate; entries must be distinct", i, ua)
		}
		seen[ua] = struct{}{}
		if !strings.Contains(ua, "Mac OS") {
			t.Fatalf("pool[%d] = %q must contain 'Mac OS' (macOS hardening / Session_id gating rely on it)", i, ua)
		}
		// Originator is forced to codexOriginator alongside the UA, so every
		// entry must be a matching "Codex Desktop/…" build.
		if !strings.HasPrefix(ua, codexOriginator+"/") {
			t.Fatalf("pool[%d] = %q must start with %q/", i, ua, codexOriginator)
		}
		if !strings.Contains(ua, "("+codexOriginator+";") {
			t.Fatalf("pool[%d] = %q must self-reference %q to stay in lockstep with Originator", i, ua, codexOriginator)
		}
	}
}

func TestCodexFallbackUserAgentIsStablePerAuthID(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "codex-alice@twobird.site-pro.json", Provider: "codex"}
	first := codexFallbackUserAgent(auth)
	for i := 0; i < 100; i++ {
		if got := codexFallbackUserAgent(auth); got != first {
			t.Fatalf("selection not stable: iteration %d got %q, first was %q", i, got, first)
		}
	}
	// Must be a real pool entry.
	found := false
	for _, ua := range codexFallbackUserAgentPool {
		if ua == first {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("selected UA %q is not in the pool", first)
	}
}

func TestCodexFallbackUserAgentSpreadsAcrossAccounts(t *testing.T) {
	ids := []string{
		"codex-alice@twobird.site-pro.json",
		"codex-ethan@twobird.site-pro.json",
		"codex-link@icodeeasy.cc-pro.json",
		"codex-codekissyoung@icloud.com-pro.json",
		"codex-torinvexley@gmail.com-pro.json",
		"codex-hantosmantv@gmail.com-pro.json",
		"codex-oat@gmail.com-pro.json",
		"codex-charlie@twobird.site-pro.json",
	}
	distinct := make(map[string]struct{})
	for _, id := range ids {
		distinct[codexFallbackUserAgent(&cliproxyauth.Auth{ID: id, Provider: "codex"})] = struct{}{}
	}
	// Not a strict guarantee (hashing can collide), but with 8 accounts over an
	// 8-entry pool we expect real spread — a single bucket would defeat the point.
	if len(distinct) < 3 {
		t.Fatalf("forced UA spread across %d accounts collapsed to %d distinct values; expected >=3", len(ids), len(distinct))
	}
}

func TestCodexFallbackUserAgentEmptyOrNilAuthUsesDefault(t *testing.T) {
	if got := codexFallbackUserAgent(nil); got != codexUserAgent {
		t.Fatalf("nil auth = %q, want codexUserAgent", got)
	}
	if got := codexFallbackUserAgent(&cliproxyauth.Auth{Provider: "codex"}); got != codexUserAgent {
		t.Fatalf("empty-ID auth = %q, want codexUserAgent", got)
	}
	if got := codexFallbackUserAgent(&cliproxyauth.Auth{ID: "   ", Provider: "codex"}); got != codexUserAgent {
		t.Fatalf("whitespace-ID auth = %q, want codexUserAgent", got)
	}
}

// End-to-end through the header builder: a non-macOS client UA on an
// identified pool auth must be rewritten to that auth's pinned pool UA, with
// Originator following in lockstep.
func TestApplyCodexHeadersForcesPooledUAForIdentifiedAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "codex-ethan@twobird.site-pro.json", Provider: "codex"}
	want := codexFallbackUserAgent(auth)

	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "codex-tui/0.144.1 (Ubuntu 26.4.0; x86_64) xterm-256color (codex-tui; 0.144.1)",
		"Originator": "codex-tui",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != want {
		t.Fatalf("User-Agent = %q, want pinned pool UA %q", got, want)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q (UA rewritten, must stay in lockstep)", got, codexOriginator)
	}
}
