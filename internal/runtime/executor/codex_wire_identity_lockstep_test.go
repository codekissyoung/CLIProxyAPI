package executor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// The executor and model override headers must move in lockstep so one account
// never presents different Codex identities across models (divergence #20).
func TestCodexWireIdentityStaysInLockstep(t *testing.T) {
	headers := registry.ModelOverrideHeaders("gpt-5.6-luna")
	if headers == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil，override_header 不该消失")
	}
	if got := headers["user-agent"]; got != codexUserAgent {
		t.Fatalf("models.json override UA = %q，executor 常量 = %q：两处必须同版本，"+
			"升 codexUserAgent 时别忘了 models.json 的 override_header", got, codexUserAgent)
	}
	if got := headers["originator"]; got != codexOriginator {
		t.Fatalf("models.json override originator = %q, want %q", got, codexOriginator)
	}

	// The UA must self-reference codexVersion so it cannot contradict the Version header.
	if !strings.Contains(codexUserAgent, codexVersion) {
		t.Fatalf("codexUserAgent %q 不含 codexVersion %q", codexUserAgent, codexVersion)
	}
	if strings.Count(codexUserAgent, codexVersion) != 2 {
		t.Fatalf("codexUserAgent %q 应在前缀和尾部自引用各出现一次 %q", codexUserAgent, codexVersion)
	}
}
