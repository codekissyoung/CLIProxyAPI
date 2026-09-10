package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func setAccountConcurrencyLimitForTest(t *testing.T, limit int) {
	t.Helper()
	SetAccountConcurrencyLimit(limit)
	t.Cleanup(func() { SetAccountConcurrencyLimit(0) })
}

func setAccountInFlightForTest(t *testing.T, authID string, count int) {
	t.Helper()
	accountConcurrencyCapacity.Lock()
	if accountConcurrencyCapacity.inFlight == nil {
		accountConcurrencyCapacity.inFlight = make(map[string]int)
	}
	accountConcurrencyCapacity.inFlight[authID] = count
	accountConcurrencyCapacity.Unlock()
	t.Cleanup(func() {
		accountConcurrencyCapacity.Lock()
		delete(accountConcurrencyCapacity.inFlight, authID)
		accountConcurrencyCapacity.Unlock()
	})
}

func waitForAccountInFlight(t *testing.T, authID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if accountInFlightCount(authID) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for in-flight(%s) = %d, got %d", authID, want, accountInFlightCount(authID))
}

func TestAccountConcurrencyGateDisabledByDefault(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 0)

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	auths := []*Auth{{ID: "cap-off-a"}, {ID: "cap-off-b"}}
	want := []string{"cap-off-a", "cap-off-b", "cap-off-a"}
	for i, wantID := range want {
		auth, errPick := selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
		if errPick != nil || auth == nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		if auth.ID != wantID {
			t.Fatalf("Pick() #%d = %q, want %q", i, auth.ID, wantID)
		}
	}

	// Tracking is a no-op while the gate is disabled.
	release := trackAuthInFlight(auths[0])
	release()
	if got := accountInFlightCount("cap-off-a"); got != 0 {
		t.Fatalf("in-flight(cap-off-a) = %d, want 0 with the gate disabled", got)
	}
}

func TestAccountConcurrencyGateFullFallsToNext(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	// Fill-first keeps the post-release pick deterministic (round-robin would
	// have advanced past auth-a during the saturated pick).
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()

	authA := &Auth{ID: "cap-full-a"}
	authB := &Auth{ID: "cap-full-b"}
	auths := []*Auth{authA, authB}

	release := trackAuthInFlight(authA)
	if got := accountInFlightCount("cap-full-a"); got != 1 {
		t.Fatalf("in-flight(cap-full-a) = %d, want 1", got)
	}

	auth, errPick := selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if errPick != nil || auth == nil {
		t.Fatalf("Pick() with full auth-a error = %v", errPick)
	}
	if auth.ID != "cap-full-b" {
		t.Fatalf("Pick() with full auth-a = %q, want cap-full-b", auth.ID)
	}

	release()
	auth, errPick = selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if errPick != nil || auth == nil {
		t.Fatalf("Pick() after release error = %v", errPick)
	}
	if auth.ID != "cap-full-a" {
		t.Fatalf("Pick() after release = %q, want cap-full-a", auth.ID)
	}
}

func TestAccountConcurrencyGateAffinityHomeFullTempThenRecover(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	authA := &Auth{ID: "cap-home-a"}
	authB := &Auth{ID: "cap-home-b"}
	auths := []*Auth{authA, authB}

	pick := func() string {
		t.Helper()
		auth, errPick := selector.Pick(context.Background(), "claude", "claude-3-7-sonnet", claudeAffinityTestOptions(), auths)
		if errPick != nil || auth == nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		return auth.ID
	}

	if got := pick(); got != "cap-home-a" {
		t.Fatalf("cold Pick() = %q, want cap-home-a", got)
	}

	// The home credential saturates: the session temp-fails over but the home
	// binding must stay intact (same semantics as the overload divergence).
	release := trackAuthInFlight(authA)
	if got := pick(); got != "cap-home-b" {
		t.Fatalf("Pick() with full home = %q, want cap-home-b temporary fallback", got)
	}
	if bound, ok := selector.cache.Get("claude::claude:sess-1"); !ok || bound != "cap-home-a" {
		t.Fatalf("home binding = %q, %v; want cap-home-a, true", bound, ok)
	}

	release()
	if got := pick(); got != "cap-home-a" {
		t.Fatalf("Pick() after slot freed = %q, want cap-home-a (home_recovered)", got)
	}
}

func TestAccountConcurrencyPerAuthOverride(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	// The attribute raises auth-a's limit above the global default: one
	// in-flight request does not saturate it, two do.
	authA := &Auth{ID: "cap-ovr-a", Attributes: map[string]string{AttributeConcurrency: "2"}}
	authB := &Auth{ID: "cap-ovr-b"}
	auths := []*Auth{authA, authB}

	releaseOne := trackAuthInFlight(authA)
	defer releaseOne()
	auth, errPick := selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if errPick != nil || auth == nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if auth.ID != "cap-ovr-a" {
		t.Fatalf("Pick() with headroom = %q, want cap-ovr-a", auth.ID)
	}

	releaseTwo := trackAuthInFlight(authA)
	defer releaseTwo()
	auth, errPick = selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if errPick != nil || auth == nil {
		t.Fatalf("Pick() with saturated override error = %v", errPick)
	}
	if auth.ID != "cap-ovr-b" {
		t.Fatalf("Pick() with saturated override = %q, want cap-ovr-b", auth.ID)
	}

	// An explicit "0" attribute opts the account out of the gate entirely.
	authC := &Auth{ID: "cap-ovr-c", Attributes: map[string]string{AttributeConcurrency: "0"}}
	setAccountInFlightForTest(t, "cap-ovr-c", 5)
	if accountCapacityBlocked(authC) {
		t.Fatal("accountCapacityBlocked() = true for an unlimited account")
	}
	auth, errPick = selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, []*Auth{authC})
	if errPick != nil || auth == nil {
		t.Fatalf("Pick() unlimited account error = %v", errPick)
	}
	if auth.ID != "cap-ovr-c" {
		t.Fatalf("Pick() unlimited account = %q, want cap-ovr-c", auth.ID)
	}
}

func TestEffectiveAccountConcurrencyLimitFallback(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 5)

	cases := []struct {
		name      string
		auth      *Auth
		wantLimit int
	}{
		{name: "nil auth", auth: nil, wantLimit: 0},
		{name: "no attributes", auth: &Auth{ID: "cap-eff-a"}, wantLimit: 5},
		{name: "valid override", auth: &Auth{ID: "cap-eff-b", Attributes: map[string]string{AttributeConcurrency: "7"}}, wantLimit: 7},
		{name: "zero override means unlimited", auth: &Auth{ID: "cap-eff-c", Attributes: map[string]string{AttributeConcurrency: "0"}}, wantLimit: 0},
		{name: "unparsable falls back", auth: &Auth{ID: "cap-eff-d", Attributes: map[string]string{AttributeConcurrency: "abc"}}, wantLimit: 5},
		{name: "empty falls back", auth: &Auth{ID: "cap-eff-e", Attributes: map[string]string{AttributeConcurrency: ""}}, wantLimit: 5},
		{name: "negative falls back", auth: &Auth{ID: "cap-eff-f", Attributes: map[string]string{AttributeConcurrency: "-2"}}, wantLimit: 5},
		{name: "whitespace padded override", auth: &Auth{ID: "cap-eff-g", Attributes: map[string]string{AttributeConcurrency: " 9 "}}, wantLimit: 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveAccountConcurrencyLimit(tc.auth); got != tc.wantLimit {
				t.Fatalf("effectiveAccountConcurrencyLimit() = %d, want %d", got, tc.wantLimit)
			}
		})
	}

	// A negative global limit clamps to disabled.
	SetAccountConcurrencyLimit(-4)
	if got := accountConcurrencyLimit(); got != 0 {
		t.Fatalf("accountConcurrencyLimit() = %d after negative set, want 0", got)
	}
	if got := effectiveAccountConcurrencyLimit(&Auth{ID: "cap-eff-h"}); got != 0 {
		t.Fatalf("effectiveAccountConcurrencyLimit() = %d with gate disabled, want 0", got)
	}
	// An explicit per-account attribute still gates while the global default is off.
	if got := effectiveAccountConcurrencyLimit(&Auth{ID: "cap-eff-i", Attributes: map[string]string{AttributeConcurrency: "3"}}); got != 3 {
		t.Fatalf("effectiveAccountConcurrencyLimit() = %d with attribute-only config, want 3", got)
	}
}

func TestTrackAuthInFlightLifecycle(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 3)

	auth := &Auth{ID: "cap-life-a"}
	releaseOne := trackAuthInFlight(auth)
	releaseTwo := trackAuthInFlight(auth)
	if got := accountInFlightCount("cap-life-a"); got != 2 {
		t.Fatalf("in-flight = %d, want 2", got)
	}
	releaseOne()
	if got := accountInFlightCount("cap-life-a"); got != 1 {
		t.Fatalf("in-flight after one release = %d, want 1", got)
	}
	releaseOne() // double release is a no-op
	if got := accountInFlightCount("cap-life-a"); got != 1 {
		t.Fatalf("in-flight after double release = %d, want 1", got)
	}
	releaseTwo()
	if got := accountInFlightCount("cap-life-a"); got != 0 {
		t.Fatalf("in-flight after full release = %d, want 0", got)
	}
	accountConcurrencyCapacity.Lock()
	_, exists := accountConcurrencyCapacity.inFlight["cap-life-a"]
	accountConcurrencyCapacity.Unlock()
	if exists {
		t.Fatal("in-flight map entry leaked after full release")
	}
}

// capacityTestExecutor executes on codex and reports the serving auth ID.
type capacityTestExecutor struct {
	streamChunks chan cliproxyexecutor.StreamChunk
}

func (e *capacityTestExecutor) Identifier() string { return "codex" }

func (e *capacityTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *capacityTestExecutor) ExecuteStream(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.streamChunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: test\n\n")}
	return &cliproxyexecutor.StreamResult{Chunks: e.streamChunks}, nil
}

func (*capacityTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (*capacityTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (*capacityTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newCapacityTestManager(t *testing.T, selector Selector, authIDs ...string) *Manager {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(&capacityTestExecutor{streamChunks: make(chan cliproxyexecutor.StreamChunk, 1)})
	registerSchedulerModels(t, "codex", "cap-model", authIDs...)
	for _, authID := range authIDs {
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "codex"}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", authID, errRegister)
		}
	}
	return manager
}

// The scheduler fast path (built-in selectors) must apply the same gate.
func TestManagerExecuteAccountConcurrencySchedulerFastPath(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	manager := newCapacityTestManager(t, &RoundRobinSelector{}, "cap-fast-a", "cap-fast-b")
	release := trackAuthInFlight(&Auth{ID: "cap-fast-a"})
	defer release()

	response, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "cap-fast-b" {
		t.Fatalf("Execute() served by %q, want cap-fast-b while cap-fast-a is saturated", got)
	}
}

// Session affinity + capacity: a saturated home fails over to a temporary
// account without unbinding, and the session migrates back once a slot frees.
func TestManagerExecuteAccountConcurrencyAffinityTempThenRecover(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	manager := newCapacityTestManager(t, selector, "cap-aff-a", "cap-aff-b")

	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{"cap-sess-1"}},
		Metadata: map[string]any{},
	}
	execute := func() string {
		t.Helper()
		response, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, opts)
		if errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		return string(response.Payload)
	}

	if got := execute(); got != "cap-aff-a" {
		t.Fatalf("cold Execute() served by %q, want cap-aff-a", got)
	}

	release := trackAuthInFlight(&Auth{ID: "cap-aff-a"})
	if got := execute(); got != "cap-aff-b" {
		t.Fatalf("Execute() with saturated home served by %q, want cap-aff-b temporary fallback", got)
	}

	release()
	if got := execute(); got != "cap-aff-a" {
		t.Fatalf("Execute() after slot freed served by %q, want cap-aff-a (home_recovered)", got)
	}
}

// A streaming request holds its capacity slot until the stream ends.
func TestManagerExecuteStreamAccountConcurrencySlotHeldUntilStreamEnd(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	manager := newCapacityTestManager(t, &RoundRobinSelector{}, "cap-str-a", "cap-str-b")
	executor := &capacityTestExecutor{streamChunks: make(chan cliproxyexecutor.StreamChunk, 1)}
	manager.RegisterExecutor(executor)

	result, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	waitForAccountInFlight(t, "cap-str-a", 1)

	if _, ok := <-result.Chunks; !ok {
		t.Fatal("stream closed before the buffered chunk")
	}
	if got := accountInFlightCount("cap-str-a"); got != 1 {
		t.Fatalf("in-flight(cap-str-a) = %d mid-stream, want 1 (slot held)", got)
	}

	close(executor.streamChunks)
	for range result.Chunks {
	}
	waitForAccountInFlight(t, "cap-str-a", 0)
}
