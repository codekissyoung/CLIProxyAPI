package helps

// ice divergence: codex turn-state injection (decision tree over the captured NORMAL ticket); upstream has no equivalent — keep on merge.

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/metrics"
	log "github.com/sirupsen/logrus"
)

// CodexTurnStateHeader is the chatgpt.com codex header carrying the opaque
// server-side turn-state ticket.
const CodexTurnStateHeader = "X-Codex-Turn-State"

// Injection decision actions. The ticket return value of
// DecideTurnStateInjection is non-empty only for "inject" and "replace".
const (
	TurnStateActionKeepClient   = "keep_client"
	TurnStateActionReplace      = "replace"
	TurnStateActionInject       = "inject"
	TurnStateActionPassNoTicket = "pass_no_ticket"
	TurnStateActionKeepUnknown  = "keep_unknown"
)

// DecideTurnStateInjection decides what to do with the client's
// X-Codex-Turn-State request header for the given account+model bucket:
//
//	client 292 chars (normal)   -> keep_client (client state is good)
//	client 312 chars (degraded) -> replace with the stored NORMAL ticket when
//	                               injectable, else keep_client
//	client header absent        -> inject the stored NORMAL ticket when
//	                               injectable, else pass_no_ticket
//	any other length            -> keep_unknown (conservative, never touched)
//
// dryRun changes nothing about the decision; it is only reported in the
// decision metric and log line. The log carries auth_id/model/action/dry_run
// only - never the blob or the client header value.
func DecideTurnStateInjection(authID, model, clientHeaderValue string, dryRun bool) (action string, ticket string) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	switch len(clientHeaderValue) {
	case turnStateNormalLength:
		action = TurnStateActionKeepClient
	case turnStateDegradedLength:
		if stored, ok := lookupInjectableTurnState(authID, model, turnStateNowFunc()); ok {
			action, ticket = TurnStateActionReplace, stored
		} else {
			action = TurnStateActionKeepClient
		}
	case 0:
		if stored, ok := lookupInjectableTurnState(authID, model, turnStateNowFunc()); ok {
			action, ticket = TurnStateActionInject, stored
		} else {
			action = TurnStateActionPassNoTicket
		}
	default:
		action = TurnStateActionKeepUnknown
	}
	metrics.RecordCodexTurnStateInjectionDecision(authID, model, action, dryRun)
	log.WithFields(log.Fields{
		"auth_id": authID,
		"model":   model,
		"action":  action,
		"dry_run": dryRun,
	}).Info("codex turn-state injection decision")
	return action, ticket
}

// ApplyTurnStateInjectionDecision applies a decision to the outbound request
// headers: in enforce mode (dryRun false) it sets the stored ticket for
// inject/replace decisions; in dry-run mode it never mutates anything. The
// client header value is only ever overwritten by a stored NORMAL ticket.
func ApplyTurnStateInjectionDecision(header http.Header, action, ticket string, dryRun bool) {
	if header == nil || dryRun || ticket == "" {
		return
	}
	if action == TurnStateActionInject || action == TurnStateActionReplace {
		header.Set(CodexTurnStateHeader, ticket)
	}
}

// lookupInjectableTurnState returns the bucket's stored NORMAL ticket when it
// is still injectable at now.
func lookupInjectableTurnState(authID, model string, now time.Time) (string, bool) {
	if authID == "" || model == "" {
		return "", false
	}
	turnStatesMu.RLock()
	entry, ok := turnStates[authID+"|"+model]
	var ticket string
	var injectable bool
	if ok {
		ticket, injectable = entry.injectableTicket(now)
	}
	turnStatesMu.RUnlock()
	return ticket, injectable
}
