package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func TestGetCodexTurnTickets_NilHandler(t *testing.T) {
	var h *Handler
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-turn-tickets", nil)

	h.GetCodexTurnTickets(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestGetCodexTurnTickets_ReturnsSnapshotWithoutBlob(t *testing.T) {
	// Seed the store through the exported writer with a unique account key and a
	// realistic blob-shaped value (Fernet tokens start with "gAAAAA") of exactly
	// the normal length; only the shape may be observable.
	fakeBlob := "gAAAAA" + strings.Repeat("b", 292-len("gAAAAA"))
	helps.ObserveTurnState("turnstate-handler-test-auth.json", "gpt-5.5", fakeBlob)

	h := &Handler{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-turn-tickets", nil)

	h.GetCodexTurnTickets(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "gAAAAA") {
		t.Fatalf("response leaks the turn-state blob: %s", body)
	}
	var payload struct {
		StartedAt string `json:"started_at"`
		Tickets   []struct {
			AuthID     string `json:"auth_id"`
			Model      string `json:"model"`
			Normal     int64  `json:"normal"`
			LastLength int    `json:"last_length"`
			LastShape  string `json:"last_shape"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.StartedAt == "" {
		t.Fatal("started_at must be set")
	}
	found := false
	for _, ticket := range payload.Tickets {
		if ticket.AuthID == "turnstate-handler-test-auth.json" && ticket.Model == "gpt-5.5" {
			found = true
			if ticket.Normal < 1 || ticket.LastLength != 292 || ticket.LastShape != "normal" {
				t.Errorf("seeded ticket = %+v, want normal count >= 1, last_length 292, last_shape normal", ticket)
			}
		}
	}
	if !found {
		t.Fatalf("seeded ticket not found in response: %s", body)
	}
}
