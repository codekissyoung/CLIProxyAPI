package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// mustAcquireAccountCapacity reserves a test slot and fails the test when the
// admission gate rejects it.
func mustAcquireAccountCapacity(t *testing.T, auth *Auth) func() {
	t.Helper()
	release, ok := tryAcquireAccountCapacity(auth)
	if !ok {
		t.Fatalf("tryAcquireAccountCapacity(%s) rejected, want admitted", auth.ID)
	}
	return release
}

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
	release := mustAcquireAccountCapacity(t, auths[0])
	release()
	if got := accountInFlightCount("cap-off-a"); got != 0 {
		t.Fatalf("in-flight(cap-off-a) = %d, want 0 with the gate disabled", got)
	}
}

func TestAccountConcurrencyGateFullFallsToNext(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	// Fill-first keeps the post-release pick deterministic (round-robin would
	// have advanced past auth-a during the saturated attempt).
	manager := newCapacityTestManager(t, &FillFirstSelector{}, "cap-full-a", "cap-full-b")

	release := mustAcquireAccountCapacity(t, &Auth{ID: "cap-full-a"})
	if got := accountInFlightCount("cap-full-a"); got != 1 {
		t.Fatalf("in-flight(cap-full-a) = %d, want 1", got)
	}

	// Admission rejects the saturated credential and the request fails over to
	// the next candidate within the same Execute call.
	response, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() with full auth-a error = %v", errExecute)
	}
	if got := string(response.Payload); got != "cap-full-b" {
		t.Fatalf("Execute() with full auth-a served by %q, want cap-full-b", got)
	}

	release()
	response, errExecute = manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() after release error = %v", errExecute)
	}
	if got := string(response.Payload); got != "cap-full-a" {
		t.Fatalf("Execute() after release served by %q, want cap-full-a", got)
	}
}

func TestAccountConcurrencyPerAuthOverride(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	// The attribute raises the account's limit above the global default: two
	// reservations fit, the third is rejected atomically.
	authA := &Auth{ID: "cap-ovr-a", Attributes: map[string]string{AttributeConcurrency: "2"}}
	releaseOne := mustAcquireAccountCapacity(t, authA)
	defer releaseOne()
	releaseTwo := mustAcquireAccountCapacity(t, authA)
	defer releaseTwo()
	if releaseThree, okThree := tryAcquireAccountCapacity(authA); okThree || releaseThree != nil {
		t.Fatal("third acquire admitted beyond the per-auth override limit")
	}

	// An explicit "0" attribute opts the account out of the gate entirely:
	// admission succeeds no matter how much load the counter already shows.
	authC := &Auth{ID: "cap-ovr-c", Attributes: map[string]string{AttributeConcurrency: "0"}}
	setAccountInFlightForTest(t, "cap-ovr-c", 5)
	releaseUnlimited := mustAcquireAccountCapacity(t, authC)
	releaseUnlimited()
	if got := accountInFlightCount("cap-ovr-c"); got != 5 {
		t.Fatalf("in-flight(cap-ovr-c) = %d, want 5 (unlimited accounts are not tracked)", got)
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

func TestTryAcquireAccountCapacityAdmission(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 2)

	auth := &Auth{ID: "cap-life-a"}
	releaseOne, okOne := tryAcquireAccountCapacity(auth)
	if !okOne {
		t.Fatal("first acquire rejected, want admitted")
	}
	releaseTwo, okTwo := tryAcquireAccountCapacity(auth)
	if !okTwo {
		t.Fatal("second acquire rejected, want admitted")
	}

	// The third reservation must be rejected atomically at the limit.
	releaseThree, okThree := tryAcquireAccountCapacity(auth)
	if okThree || releaseThree != nil {
		t.Fatal("third acquire admitted beyond the limit")
	}
	if got := accountInFlightCount("cap-life-a"); got != 2 {
		t.Fatalf("in-flight after rejected acquire = %d, want 2 (no phantom slot)", got)
	}
	if got := accountInFlightHighWater("cap-life-a"); got != 2 {
		t.Fatalf("high-water = %d, want 2", got)
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

	// A freed slot is reservable again.
	releaseFour, okFour := tryAcquireAccountCapacity(auth)
	if !okFour {
		t.Fatal("acquire after release rejected, want admitted")
	}
	releaseFour()
}

// 50 goroutines race for limit=5 slots on one credential behind a channel
// barrier. The high-water mark must never exceed the limit and the counter
// must drain to zero.
func TestTryAcquireAccountCapacityConcurrentStorm(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 5)

	auth := &Auth{ID: "cap-storm-a"}
	const contenders = 50
	start := make(chan struct{})
	outcome := make(chan bool, contenders) // true = acquired
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, ok := tryAcquireAccountCapacity(auth)
			if ok {
				release()
			}
			outcome <- ok
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent storm deadlocked")
	}

	acquired := 0
	for range contenders {
		if <-outcome {
			acquired++
		}
	}
	if acquired == 0 {
		t.Fatal("no contender ever acquired a slot")
	}
	if got := accountInFlightHighWater("cap-storm-a"); got > 5 {
		t.Fatalf("high-water = %d, exceeds limit 5", got)
	}
	if got := accountInFlightCount("cap-storm-a"); got != 0 {
		t.Fatalf("in-flight after storm = %d, want 0 (leak)", got)
	}
}

// capacityTestExecutor executes on codex and reports the serving auth ID.
type capacityTestExecutor struct {
	streamChunks chan cliproxyexecutor.StreamChunk
	// Optional blocking mode for admission stress tests: Execute signals
	// started and holds the request until releaseAll closes.
	started    chan string
	releaseAll chan struct{}
}

func (e *capacityTestExecutor) Identifier() string { return "codex" }

func (e *capacityTestExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.releaseAll != nil {
		e.started <- auth.ID
		select {
		case <-ctx.Done():
			return cliproxyexecutor.Response{}, ctx.Err()
		case <-e.releaseAll:
		}
	}
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
	release := mustAcquireAccountCapacity(t, &Auth{ID: "cap-fast-a"})
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

	release := mustAcquireAccountCapacity(t, &Auth{ID: "cap-aff-a"})
	if got := execute(); got != "cap-aff-b" {
		t.Fatalf("Execute() with saturated home served by %q, want cap-aff-b temporary fallback", got)
	}
	// The saturated home keeps its binding: the failover is temporary, exactly
	// like the overload divergence.
	if bound, ok := selector.cache.Get("mixed::claude:cap-sess-1"); !ok || bound != "cap-aff-a" {
		t.Fatalf("home binding = %q, %v; want cap-aff-a, true", bound, ok)
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

// End-to-end admission storm: 50 concurrent requests against a single account
// capped at 5 in-flight slots. Exactly 5 win a slot and block in the executor;
// the other 45 must fail over to a retryable capacity-busy error. The counter
// never exceeds the limit and drains to zero afterwards.
func TestManagerExecuteAccountConcurrencyAdmissionStorm(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 5)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	executor := &capacityTestExecutor{
		streamChunks: make(chan cliproxyexecutor.StreamChunk, 1),
		started:      make(chan string, 64),
		releaseAll:   make(chan struct{}),
	}
	manager.RegisterExecutor(executor)
	registerSchedulerModels(t, "codex", "cap-model", "cap-storm-mgr-a")
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "cap-storm-mgr-a", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	const contenders = 50
	const limit = 5
	type executeOutcome struct {
		payload string
		err     error
	}
	start := make(chan struct{})
	outcomes := make(chan executeOutcome, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
			outcomes <- executeOutcome{payload: string(response.Payload), err: errExecute}
		}()
	}
	close(start)

	readOutcome := func() executeOutcome {
		t.Helper()
		select {
		case outcome := <-outcomes:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for an execute outcome (deadlock?)")
			return executeOutcome{}
		}
	}

	// The 5 slot holders block inside the executor, so every other contender
	// deterministically observes a full account and fails busy.
	busy := 0
	for busy < contenders-limit {
		outcome := readOutcome()
		var busyErr *accountConcurrencyBusyError
		if !errors.As(outcome.err, &busyErr) || busyErr == nil {
			t.Fatalf("outcome %d: err = %v, want accountConcurrencyBusyError (payload %q)", busy, outcome.err, outcome.payload)
		}
		if busyErr.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("busy status = %d, want 429", busyErr.StatusCode())
		}
		if got := SafeResponseHeaders(outcome.err).Get("Retry-After"); got != "1" {
			t.Fatalf("busy Retry-After = %q, want 1", got)
		}
		busy++
	}
	if got := accountInFlightCount("cap-storm-mgr-a"); got != limit {
		t.Fatalf("in-flight with holders blocked = %d, want %d", got, limit)
	}

	close(executor.releaseAll)
	for range limit {
		outcome := readOutcome()
		if outcome.err != nil {
			t.Fatalf("holder outcome err = %v, want success", outcome.err)
		}
		if outcome.payload != "cap-storm-mgr-a" {
			t.Fatalf("holder served by %q, want cap-storm-mgr-a", outcome.payload)
		}
	}

	waitForAccountInFlight(t, "cap-storm-mgr-a", 0)
	if got := accountInFlightHighWater("cap-storm-mgr-a"); got != limit {
		t.Fatalf("high-water = %d, want exactly %d", got, limit)
	}
	if got := accountInFlightCount("cap-storm-mgr-a"); got != 0 {
		t.Fatalf("in-flight after storm = %d, want 0 (leak)", got)
	}
	wg.Wait()
}

// Review minor: when one credential is capacity-saturated and the rest are in
// cooldown, the final error must be the cooldown reason, not the 429 capacity
// busy error (busy is only truthful when capacity is the sole failure cause).
func TestManagerExecuteAccountConcurrencyBusyYieldsToCooldown(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&capacityTestExecutor{streamChunks: make(chan cliproxyexecutor.StreamChunk, 1)})
	registerSchedulerModels(t, "codex", "cap-model", "cap-mix-a", "cap-mix-b")
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "cap-mix-a", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register(cap-mix-a) error = %v", errRegister)
	}
	cooling := &Auth{ID: "cap-mix-b", Provider: "codex", Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}
	if _, errRegister := manager.Register(context.Background(), cooling); errRegister != nil {
		t.Fatalf("Register(cap-mix-b) error = %v", errRegister)
	}

	release := mustAcquireAccountCapacity(t, &Auth{ID: "cap-mix-a"})
	defer release()

	_, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want cooldown failure")
	}
	var busyErr *accountConcurrencyBusyError
	if errors.As(errExecute, &busyErr) {
		t.Fatalf("Execute() error = %v, want the cooldown reason (busy must not mask it)", errExecute)
	}
	var cooldownErr *modelCooldownError
	var authErr *Error
	isCooldown := errors.As(errExecute, &cooldownErr) && cooldownErr != nil
	isUnavailable := errors.As(errExecute, &authErr) && authErr != nil && authErr.Code == "auth_unavailable"
	if !isCooldown && !isUnavailable {
		t.Fatalf("Execute() error = %v (%T), want modelCooldownError or auth_unavailable", errExecute, errExecute)
	}
}

// When every candidate is capacity-saturated, the busy error remains the final
// answer (429 + Retry-After), since capacity is the sole failure cause.
func TestManagerExecuteAccountConcurrencyAllFullReturnsBusy(t *testing.T) {
	setAccountConcurrencyLimitForTest(t, 1)

	manager := newCapacityTestManager(t, &RoundRobinSelector{}, "cap-all-a", "cap-all-b")
	releaseA := mustAcquireAccountCapacity(t, &Auth{ID: "cap-all-a"})
	defer releaseA()
	releaseB := mustAcquireAccountCapacity(t, &Auth{ID: "cap-all-b"})
	defer releaseB()

	_, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "cap-model"}, cliproxyexecutor.Options{})
	var busyErr *accountConcurrencyBusyError
	if !errors.As(errExecute, &busyErr) || busyErr == nil {
		t.Fatalf("Execute() error = %v, want accountConcurrencyBusyError", errExecute)
	}
	if busyErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("busy status = %d, want 429", busyErr.StatusCode())
	}
	if got := SafeResponseHeaders(errExecute).Get("Retry-After"); got != "1" {
		t.Fatalf("busy Retry-After = %q, want 1", got)
	}
}
