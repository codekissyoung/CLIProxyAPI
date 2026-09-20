package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// GetCodexTurnTickets returns the passive X-Codex-Turn-State shape observations
// collected since process start, keyed by account and model. Shape metadata
// only: the opaque header blob is never captured, stored, or exposed.
func (h *Handler) GetCodexTurnTickets(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	c.JSON(http.StatusOK, helps.SnapshotTurnStates())
}
