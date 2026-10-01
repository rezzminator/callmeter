// Package clock is the one time seam: every bare time.Now/time.Sleep/
// time.After in non-test code routes through a Clock instead. Real drives the
// wall clock exactly as the standard library always has; Fake drives a
// deterministic one a test advances by hand, so a sleep or an After fires on
// the test's own schedule instead of the real one.
package clock

import (
	"context"
	"time"
)

// Clock is the seam every timing door crosses instead of the bare time
// package: Now for a timestamp, Sleep for a bounded, cancellable wait, After
// for the standard library's channel-based wait.
type Clock interface {
	// Now reports the current time.
	Now() time.Time
	// Sleep blocks for d, or until ctx is done — whichever comes first —
	// returning ctx.Err() only in the latter case. Unlike time.Sleep, a
	// cancelled caller is never left blocked past its own deadline.
	Sleep(ctx context.Context, d time.Duration) error
	// After is time.After: a channel that receives once, d after this call.
	After(d time.Duration) <-chan time.Time
}
