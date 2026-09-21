package helps

// ice divergence: codex turn-state passive capture + injection (shape metadata is all that is ever logged, exported, or exposed; the blob is retained in-memory only in its NORMAL shape as an injection ticket); upstream has no equivalent — keep on merge.

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/metrics"
)

const (
	// turnStateNormalLength is the X-Codex-Turn-State header length observed for
	// a healthy server-side turn state.
	turnStateNormalLength = 292
	// turnStateDegradedLength is the header length observed for a degraded
	// server-side turn state.
	turnStateDegradedLength = 312
)

const (
	// turnStateTicketTTL is the assumed server-side validity of a captured
	// NORMAL ticket. The header carries no readable expiry (the blob is an
	// opaque Fernet token), so the TTL is an operational assumption.
	turnStateTicketTTL = time.Hour
	// turnStateInjectMinRemaining is the safety margin: a stored ticket is only
	// injectable while its remaining assumed TTL exceeds this margin.
	turnStateInjectMinRemaining = 10 * time.Minute
)

const (
	TurnStateShapeAbsent   = "absent"
	TurnStateShapeNormal   = "normal"
	TurnStateShapeDegraded = "degraded"
	TurnStateShapeOther    = "other"
)

// classifyTurnState maps the X-Codex-Turn-State header length onto a bounded
// shape class. chatgpt.com codex responses carry the header as an opaque
// Fernet blob ("gAAAAA" prefix) whose length encodes the server-side turn
// state. Only the length class is used for observability; the blob itself is
// retained solely as the bucket's injection ticket and is never logged,
// exported, or marshaled.
func classifyTurnState(length int) string {
	switch length {
	case 0:
		return TurnStateShapeAbsent
	case turnStateNormalLength:
		return TurnStateShapeNormal
	case turnStateDegradedLength:
		return TurnStateShapeDegraded
	default:
		return TurnStateShapeOther
	}
}

// CodexTurnStateTicket is one account+model observation bucket. Its exported
// fields carry shape metadata only: per-shape counts, the last observed length
// and class, and first/last observation timestamps. The unexported fields
// retain the last NORMAL-shape blob for injection; they are never marshaled,
// logged, or exported, and SnapshotTurnStates zeroes them in its copies.
type CodexTurnStateTicket struct {
	AuthID     string    `json:"auth_id"`
	Model      string    `json:"model"`
	Normal     int64     `json:"normal"`
	Degraded   int64     `json:"degraded"`
	Other      int64     `json:"other"`
	Absent     int64     `json:"absent"`
	LastLength int       `json:"last_length"`
	LastShape  string    `json:"last_shape"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`

	// normalValue is the last observed NORMAL-shape header value, kept as the
	// injection ticket for this bucket. Unexported on purpose: it must never
	// appear in the management snapshot JSON or any log line.
	normalValue string
	// normalCapturedAt is when normalValue was observed (via turnStateNowFunc).
	normalCapturedAt time.Time
}

// injectableTicket returns the stored NORMAL ticket while its assumed TTL has
// more than turnStateInjectMinRemaining left at now. Tickets are reusable and
// never consumed by injection. Caller must hold at least the read lock.
func (t *CodexTurnStateTicket) injectableTicket(now time.Time) (string, bool) {
	if t.normalValue == "" || t.normalCapturedAt.IsZero() {
		return "", false
	}
	if t.normalCapturedAt.Add(turnStateTicketTTL).Sub(now) <= turnStateInjectMinRemaining {
		return "", false
	}
	return t.normalValue, true
}

// CodexTurnStateSnapshot is a deep copy of the turn-state store at a point in
// time, safe to serialize and mutate by the caller.
type CodexTurnStateSnapshot struct {
	StartedAt time.Time              `json:"started_at"`
	Tickets   []CodexTurnStateTicket `json:"tickets"`
}

var (
	turnStatesMu        sync.RWMutex
	turnStates          = make(map[string]*CodexTurnStateTicket)
	turnStatesStartedAt = time.Now()
	// turnStateNowFunc is injectable so tests can control observation
	// timestamps deterministically instead of sleeping.
	turnStateNowFunc = time.Now
)

// ObserveTurnState records the shape of one X-Codex-Turn-State response header
// for the given account and model. Only a NORMAL shape also retains the blob
// itself (unexported, as the bucket's injection ticket) together with its
// capture time; degraded/other/absent observations never touch the stored
// ticket. Empty authID or model observations are skipped.
func ObserveTurnState(authID, model, headerValue string) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return
	}
	length := len(headerValue)
	shape := classifyTurnState(length)
	metrics.RecordCodexTurnStateObservation(authID, model, shape)

	now := turnStateNowFunc()
	key := authID + "|" + model
	turnStatesMu.Lock()
	entry, ok := turnStates[key]
	if !ok {
		entry = &CodexTurnStateTicket{
			AuthID:    authID,
			Model:     model,
			FirstSeen: now,
		}
		turnStates[key] = entry
	}
	switch shape {
	case TurnStateShapeNormal:
		entry.Normal++
		entry.normalValue = headerValue
		entry.normalCapturedAt = now
	case TurnStateShapeDegraded:
		entry.Degraded++
	case TurnStateShapeAbsent:
		entry.Absent++
	default:
		entry.Other++
	}
	entry.LastLength = length
	entry.LastShape = shape
	entry.LastSeen = now
	turnStatesMu.Unlock()
}

// SnapshotTurnStates returns a deep copy of the store: mutating the result
// does not affect the live buckets. Stored injection tickets are zeroed in the
// copy, so a snapshot can never carry the blob. The store has no TTL or
// eviction; its cardinality is bounded by accounts x models and it resets on
// restart.
func SnapshotTurnStates() CodexTurnStateSnapshot {
	turnStatesMu.RLock()
	snapshot := CodexTurnStateSnapshot{
		StartedAt: turnStatesStartedAt,
		Tickets:   make([]CodexTurnStateTicket, 0, len(turnStates)),
	}
	for _, entry := range turnStates {
		ticket := *entry
		ticket.normalValue = ""
		ticket.normalCapturedAt = time.Time{}
		snapshot.Tickets = append(snapshot.Tickets, ticket)
	}
	turnStatesMu.RUnlock()
	sort.Slice(snapshot.Tickets, func(i, j int) bool {
		if snapshot.Tickets[i].AuthID != snapshot.Tickets[j].AuthID {
			return snapshot.Tickets[i].AuthID < snapshot.Tickets[j].AuthID
		}
		return snapshot.Tickets[i].Model < snapshot.Tickets[j].Model
	})
	return snapshot
}
