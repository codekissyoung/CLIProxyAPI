package helps

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestRecordAPIResponseMetadataStoresHeadersWhenRequestLogDisabled(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	headers := http.Header{}
	headers.Add("X-Upstream-Request-Id", "upstream-req-1")

	RecordAPIResponseMetadata(ctx, &config.Config{}, http.StatusOK, headers)
	headers.Set("X-Upstream-Request-Id", "mutated")

	got := logging.GetResponseHeaders(ctx)
	if got.Get("X-Upstream-Request-Id") != "upstream-req-1" {
		t.Fatalf("response header = %q, want %q", got.Get("X-Upstream-Request-Id"), "upstream-req-1")
	}
}

func TestLogUpstreamErrorDetailOmitsEmptyOptionalFields(t *testing.T) {
	logger, hook := logtest.NewNullLogger()
	entry := logger.WithField("request_id", "req-1")

	LogUpstreamErrorDetail(entry, UpstreamErrorDetail{
		Provider: "codex",
		Model:    "gpt-5.5",
		AuthID:   "a.json",
		Status:   400,
		ErrorMsg: "context_too_large",
		// SessionID / Transport / ReqBytes intentionally left empty/zero.
	})

	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}
	e := hook.LastEntry()
	if _, ok := e.Data["session"]; ok {
		t.Fatalf("empty session should be omitted")
	}
	if _, ok := e.Data["transport"]; ok {
		t.Fatalf("empty transport should be omitted")
	}
	if _, ok := e.Data["req_bytes"]; ok {
		t.Fatalf("zero req_bytes should be omitted")
	}
	if e.Data["http_status"] != 400 {
		t.Fatalf("http_status = %v, want 400", e.Data["http_status"])
	}
}

func TestLogUpstreamErrorDetailIncludesPopulatedFields(t *testing.T) {
	logger, hook := logtest.NewNullLogger()
	entry := logger.WithField("request_id", "req-2")

	LogUpstreamErrorDetail(entry, UpstreamErrorDetail{
		Provider:  "codex",
		Model:     "gpt-5.5",
		AuthID:    "a.json",
		Status:    400,
		ErrorMsg:  "context_too_large",
		SessionID: "codex:019e9570",
		Transport: "ws",
		ReqBytes:  2159040,
	})

	e := hook.LastEntry()
	if e.Data["session"] != "codex:019e9570" {
		t.Fatalf("session = %v", e.Data["session"])
	}
	if e.Data["transport"] != "ws" {
		t.Fatalf("transport = %v", e.Data["transport"])
	}
	if e.Data["req_bytes"] != 2159040 {
		t.Fatalf("req_bytes = %v", e.Data["req_bytes"])
	}
}
