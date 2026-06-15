package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsVersionField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 9, 11, 10, 2, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "fetched latest antigravity version"
	entry.Data["version"] = "2.1.0"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("formatted line %q missing version field", line)
	}
}

// TestLogFormatterPrintsUpstreamErrorFields guards that the structured fields
// emitted by LogUpstreamErrorDetail (error_msg, session, req_bytes, http_status,
// transport, auth_id/auth_label) are rendered. They were silently dropped before
// because they were absent from logFieldOrder, which hid the real upstream error
// message (e.g. context_too_large) from the file logs.
func TestLogFormatterPrintsUpstreamErrorFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 15, 19, 0, 0, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "upstream provider error"
	entry.Data["error_msg"] = "context_too_large"
	entry.Data["session"] = "codex:abc-123"
	entry.Data["req_bytes"] = 24371517
	entry.Data["http_status"] = 400
	entry.Data["transport"] = "http"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"error_msg=context_too_large",
		"session=codex:abc-123",
		"req_bytes=24371517",
		"http_status=400",
		"transport=http",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing field %q", line, want)
		}
	}
}
