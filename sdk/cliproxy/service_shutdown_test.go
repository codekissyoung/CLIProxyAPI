package cliproxy

import (
	"context"
	"testing"
	"time"
)

func TestNewGracefulShutdownContext_IgnoresAlreadyCanceledParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := newGracefulShutdownContext(parent, 30*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("expected graceful shutdown context to remain usable after parent cancellation, got err=%v", ctx.Err())
	default:
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected graceful shutdown context to have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 25*time.Second || remaining > 31*time.Second {
		t.Fatalf("expected roughly 30s shutdown timeout, got %v", remaining)
	}
}

func TestNewGracefulShutdownContext_RespectsEarlierActiveDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelParent()

	ctx, cancel := newGracefulShutdownContext(parent, 30*time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected graceful shutdown context to have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 3*time.Second {
		t.Fatalf("expected graceful shutdown context to keep earlier deadline, got %v", remaining)
	}
}
