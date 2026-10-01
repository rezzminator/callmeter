package clock

import (
	"context"
	"time"
)

// Real is the Clock every production caller gets by default: it drives the
// wall clock exactly the way a bare time.Now/time.Sleep/time.After call always
// has.
var Real Clock = realClock{}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
