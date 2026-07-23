package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const xaiConcurrencyTestModel = "grok-4.5"

type xaiConcurrencyTestExecutor struct {
	executeStarted chan string
	executeRelease chan struct{}
	streamStarted  chan chan cliproxyexecutor.StreamChunk
}

func newXAIConcurrencyTestExecutor() *xaiConcurrencyTestExecutor {
	return &xaiConcurrencyTestExecutor{
		executeStarted: make(chan string, 8),
		executeRelease: make(chan struct{}, 8),
		streamStarted:  make(chan chan cliproxyexecutor.StreamChunk, 8),
	}
}

func (*xaiConcurrencyTestExecutor) Identifier() string { return "xai" }

func (e *xaiConcurrencyTestExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeStarted <- auth.ID
	select {
	case <-ctx.Done():
		return cliproxyexecutor.Response{}, ctx.Err()
	case <-e.executeRelease:
		return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}
}

func (e *xaiConcurrencyTestExecutor) ExecuteStream(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: test\n\n")}
	e.streamStarted <- chunks
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*xaiConcurrencyTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*xaiConcurrencyTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (*xaiConcurrencyTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newXAIOAuthConcurrencyTestManager(t *testing.T, limit int) (*Manager, *xaiConcurrencyTestExecutor) {
	t.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{XAIOAuthMaxConcurrency: limit})
	executor := newXAIConcurrencyTestExecutor()
	manager.RegisterExecutor(executor)
	authID := "xai-oauth-1"
	registerSchedulerModels(t, "xai", xaiConcurrencyTestModel, authID)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:         authID,
		Provider:   "xai",
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return manager, executor
}

func waitForXAISignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForXAIExecutionStart(t *testing.T, signal <-chan string) string {
	t.Helper()
	select {
	case authID := <-signal:
		return authID
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for xAI execution start")
		return ""
	}
}

func assertXAIOAuthConcurrencyBusy(t *testing.T, err error) {
	t.Helper()
	var busy *xaiOAuthConcurrencyBusyError
	if !errors.As(err, &busy) || busy == nil {
		t.Fatalf("error = %v, want xaiOAuthConcurrencyBusyError", err)
	}
	if busy.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", busy.StatusCode(), http.StatusTooManyRequests)
	}
	if got := SafeResponseHeaders(err).Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
}

func TestManagerExecuteXAIOAuthConcurrencyLimitAndRelease(t *testing.T) {
	manager, executor := newXAIOAuthConcurrencyTestManager(t, 3)
	request := cliproxyexecutor.Request{Model: xaiConcurrencyTestModel}
	results := make(chan error, 4)

	for range 3 {
		go func() {
			_, errExecute := manager.Execute(context.Background(), []string{"xai"}, request, cliproxyexecutor.Options{})
			results <- errExecute
		}()
	}
	for range 3 {
		waitForXAIExecutionStart(t, executor.executeStarted)
	}

	_, errBusy := manager.Execute(context.Background(), []string{"xai"}, request, cliproxyexecutor.Options{})
	assertXAIOAuthConcurrencyBusy(t, errBusy)

	executor.executeRelease <- struct{}{}
	select {
	case errExecute := <-results:
		if errExecute != nil {
			t.Fatalf("released execution error = %v", errExecute)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for released execution")
	}

	go func() {
		_, errExecute := manager.Execute(context.Background(), []string{"xai"}, request, cliproxyexecutor.Options{})
		results <- errExecute
	}()
	waitForXAIExecutionStart(t, executor.executeStarted)

	for range 3 {
		executor.executeRelease <- struct{}{}
	}
	for range 3 {
		select {
		case errExecute := <-results:
			if errExecute != nil {
				t.Fatalf("execution error = %v", errExecute)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for xAI execution completion")
		}
	}
}

func TestManagerExecuteXAIOAuthConcurrencySkipsBusyCredential(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{XAIOAuthMaxConcurrency: 1})
	executor := newXAIConcurrencyTestExecutor()
	manager.RegisterExecutor(executor)
	registerSchedulerModels(t, "xai", xaiConcurrencyTestModel, "xai-oauth-1", "xai-oauth-2")
	for _, authID := range []string{"xai-oauth-1", "xai-oauth-2"} {
		if _, errRegister := manager.Register(context.Background(), &Auth{
			ID:         authID,
			Provider:   "xai",
			Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
		}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", authID, errRegister)
		}
	}

	releaseFirst, acquiredFirst := manager.acquireXAIOAuthConcurrency(&Auth{
		ID:         "xai-oauth-1",
		Provider:   "xai",
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
	})
	if !acquiredFirst {
		t.Fatal("failed to reserve first xAI OAuth credential")
	}
	defer releaseFirst()

	executor.executeRelease <- struct{}{}
	_, errExecute := manager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: xaiConcurrencyTestModel}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if selected := waitForXAIExecutionStart(t, executor.executeStarted); selected != "xai-oauth-2" {
		t.Fatalf("selected auth = %q, want xai-oauth-2", selected)
	}
}

func TestManagerExecuteStreamXAIOAuthConcurrencyHeldUntilClose(t *testing.T) {
	manager, executor := newXAIOAuthConcurrencyTestManager(t, 3)
	request := cliproxyexecutor.Request{Model: xaiConcurrencyTestModel}
	streams := make([]*cliproxyexecutor.StreamResult, 0, 4)
	sources := make([]chan cliproxyexecutor.StreamChunk, 0, 4)

	for range 3 {
		result, errStream := manager.ExecuteStream(context.Background(), []string{"xai"}, request, cliproxyexecutor.Options{Stream: true})
		if errStream != nil {
			t.Fatalf("ExecuteStream() error = %v", errStream)
		}
		streams = append(streams, result)
		sources = append(sources, <-executor.streamStarted)
	}

	_, errBusy := manager.ExecuteStream(context.Background(), []string{"xai"}, request, cliproxyexecutor.Options{Stream: true})
	assertXAIOAuthConcurrencyBusy(t, errBusy)

	firstClosed := make(chan struct{})
	go func() {
		for range streams[0].Chunks {
		}
		close(firstClosed)
	}()
	close(sources[0])
	waitForXAISignal(t, firstClosed, "first xAI stream close")

	replacement, errReplacement := manager.ExecuteStream(context.Background(), []string{"xai"}, request, cliproxyexecutor.Options{Stream: true})
	if errReplacement != nil {
		t.Fatalf("replacement ExecuteStream() error = %v", errReplacement)
	}
	streams = append(streams, replacement)
	sources = append(sources, <-executor.streamStarted)

	for index := 1; index < len(sources); index++ {
		go func(result *cliproxyexecutor.StreamResult) {
			for range result.Chunks {
			}
		}(streams[index])
		close(sources[index])
	}
}

func TestAcquireXAIOAuthConcurrencyDoesNotLimitAPIKeys(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{XAIOAuthMaxConcurrency: 3})
	auth := &Auth{ID: "xai-api-key", Provider: "xai", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}
	releases := make([]func(), 0, 4)
	for range 4 {
		release, acquired := manager.acquireXAIOAuthConcurrency(auth)
		if !acquired {
			t.Fatal("xAI API key was unexpectedly concurrency limited")
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

func TestManagerDoesNotRetryXAIOAuthConcurrencyBusy(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	wait, retry := manager.shouldRetryAfterError(newXAIOAuthConcurrencyBusyError(), 0, []string{"xai"}, xaiConcurrencyTestModel, 30*time.Second)
	if retry || wait != 0 {
		t.Fatalf("busy retry = (%v, %t), want (0, false)", wait, retry)
	}
}
