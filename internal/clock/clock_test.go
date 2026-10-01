package clock

import (
	"testing"
	"time"
)

// Compile-time assertions that Real and Fake satisfy Clock — the whole point
// of the interface is that a caller can hold either without knowing which is
// underneath.
var (
	_ Clock = Real
	_ Clock = (*Fake)(nil)
)

// TestNewFakeReadsTheGivenStart pins that a fresh Fake reads back exactly the
// start time it was given, not a zero value or the real wall clock.
func TestNewFakeReadsTheGivenStart(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := NewFake(start)
	if got := fake.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
}
