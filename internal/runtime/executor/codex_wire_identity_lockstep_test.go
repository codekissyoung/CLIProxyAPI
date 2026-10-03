package executor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// 2026-10-03：ba1ec37c 把 executor 的 wire identity 升到 0.160.0，但 models.json 里
// 带 override_header 的四个模型还留着 0.155.1——同一个账号会在不同模型间切 UA，
// 正是 docs/ice-divergences.md #20 要求避免的那种跨模型指纹不一致。
// 这条测试让两处只能一起改。
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

	// UA 内自引用的版本号必须等于 codexVersion（Version 头用的是同一个常量），
	// 否则上游看到的 UA 版本和 Version 头会互相矛盾。
	if !strings.Contains(codexUserAgent, codexVersion) {
		t.Fatalf("codexUserAgent %q 不含 codexVersion %q", codexUserAgent, codexVersion)
	}
	if strings.Count(codexUserAgent, codexVersion) != 2 {
		t.Fatalf("codexUserAgent %q 应在前缀和尾部自引用各出现一次 %q", codexUserAgent, codexVersion)
	}
}
