package clock

import (
	"context"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestFakeAdvanceFiresDueWaitersInDueOrder proves the deterministic
// ordering Advance promises: two After channels due at different times both
// fire once Advance crosses the later one, and each carries its OWN due
// time — not the clock's start, not each other's — so a caller can tell
// which fired first from the value alone.
func TestFakeAdvanceFiresDueWaitersInDueOrder(t *testing.T) {
	fake := NewFake(epoch)
	early := fake.After(5 * time.Millisecond)
	late := fake.After(10 * time.Millisecond)

	fake.Advance(10 * time.Millisecond)

	var earlyFired, lateFired time.Time
	select {
	case earlyFired = <-early:
	default:
		t.Fatal("early After did not fire by Advance(10ms)")
	}
	select {
	case lateFired = <-late:
	default:
		t.Fatal("late After did not fire by Advance(10ms)")
	}
	if !earlyFired.Equal(epoch.Add(5 * time.Millisecond)) {
		t.Fatalf("early fired at %v, want %v", earlyFired, epoch.Add(5*time.Millisecond))
	}
	if !lateFired.Equal(epoch.Add(10 * time.Millisecond)) {
		t.Fatalf("late fired at %v, want %v", lateFired, epoch.Add(10*time.Millisecond))
	}
	if !earlyFired.Before(lateFired) {
		t.Fatalf("early (%v) did not fire before late (%v)", earlyFired, lateFired)
	}
	if got := fake.Now(); !got.Equal(epoch.Add(10 * time.Millisecond)) {
		t.Fatalf("Now() = %v after Advance(10ms), want %v", got, epoch.Add(10*time.Millisecond))
	}
}

// TestFakeAdvancePartwayLeavesTheLaterWaiterPending proves Advance only
// fires what is actually due: a waiter past the new time delivers nothing
// until a further Advance reaches it.
func TestFakeAdvancePartwayLeavesTheLaterWaiterPending(t *testing.T) {
	fake := NewFake(epoch)
	early := fake.After(5 * time.Millisecond)
	late := fake.After(50 * time.Millisecond)

	fake.Advance(5 * time.Millisecond)

	select {
	case <-early:
	default:
		t.Fatal("early After did not fire by Advance(5ms)")
	}
	select {
	case v := <-late:
		t.Fatalf("late After fired early with %v, want still pending", v)
	default:
	}

	fake.Advance(45 * time.Millisecond)
	select {
	case <-late:
	default:
		t.Fatal("late After did not fire after the second Advance reached it")
	}
}

// TestFakeAfterZeroFiresWithoutAnAdvance proves a zero wait needs no nudge,
// the way time.After(0) needs none.
func TestFakeAfterZeroFiresWithoutAnAdvance(t *testing.T) {
	fake := NewFake(epoch)
	select {
	case got := <-fake.After(0):
		if !got.Equal(epoch) {
			t.Fatalf("After(0) delivered %v, want %v", got, epoch)
		}
	default:
		t.Fatal("After(0) did not fire without an Advance")
	}
}

// TestFakeSleepBlocksUntilAdvanceReleasesIt proves the promised contract: a
// goroutine calling Sleep on a Fake blocks until another goroutine calls
// Advance past its duration.
func TestFakeSleepBlocksUntilAdvanceReleasesIt(t *testing.T) {
	fake := NewFake(epoch)
	done := make(chan error, 1)
	go func() {
		done <- fake.Sleep(context.Background(), 20*time.Millisecond)
	}()

	select {
	case err := <-done:
		t.Fatalf("Sleep returned (%v) before any Advance call", err)
	case <-time.After(20 * time.Millisecond):
		// Real wall-clock time passing must not release a Fake sleep —
		// this is the assertion, not a flake: the goroutine above must
		// still be blocked here.
	}

	fake.Advance(20 * time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sleep() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Sleep did not return within 1s of the releasing Advance")
	}
}

// TestFakeSleepReturnsCtxErrOnCancellationWithoutAnyAdvance proves the
// other half of Sleep's contract: a caller need not wait for Advance at
// all when its context is cancelled first.
func TestFakeSleepReturnsCtxErrOnCancellationWithoutAnyAdvance(t *testing.T) {
	fake := NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- fake.Sleep(ctx, time.Hour)
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Sleep() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Sleep did not return within 1s of ctx cancellation")
	}
}
