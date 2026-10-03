package clock

import (
	"context"
	"testing"
	"time"
)

func TestRealNowAdvancesWithTheWallClock(t *testing.T) {
	first := Real.Now()
	time.Sleep(time.Millisecond)
	second := Real.Now()
	if !second.After(first) {
		t.Fatalf("Real.Now() did not advance: first=%v second=%v", first, second)
	}
}

func TestRealSleepReturnsWhenCtxIsCancelledBeforeTheDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Real.Sleep(ctx, time.Hour)
	if err == nil {
		t.Fatal("Sleep with an already-cancelled ctx returned nil error, want ctx.Err()")
	}
}

func TestRealSleepReturnsAfterItsDurationOnAnUncancelledCtx(t *testing.T) {
	if err := Real.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep() error = %v", err)
	}
}

func TestRealAfterFiresOnItsOwnChannel(t *testing.T) {
	select {
	case <-Real.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("Real.After never fired within 1s")
	}
}
