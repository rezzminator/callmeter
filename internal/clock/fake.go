package clock

import (
	"context"
	"sync"
	"time"
)

// Fake is a deterministic Clock: time moves only when a test calls Advance,
// and every Sleep/After registered against it fires in due-time order
// (registration order breaks a tie) as Advance crosses each one's deadline.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
	nextID  uint64
}

type fakeWaiter struct {
	id      uint64
	due     time.Time
	channel chan time.Time
	active  bool
}

// NewFake returns a Fake clock reading start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Sleep blocks until Advance carries the fake clock past d, or until ctx is
// done, whichever happens first — a goroutine calling this must be released
// by an Advance from another goroutine, exactly the shape a test drives a
// blocked worker with.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	channel := f.After(d)
	select {
	case <-channel:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerLocked(d).channel
}

func (f *Fake) registerLocked(delay time.Duration) *fakeWaiter {
	f.nextID++
	waiter := &fakeWaiter{
		id:      f.nextID,
		due:     f.now.Add(delay),
		channel: make(chan time.Time, 1),
		active:  true,
	}
	f.waiters = append(f.waiters, waiter)
	if delay <= 0 {
		// A zero or negative delay fires immediately, the same way
		// time.After(0) needs no external nudge — a Fake standing in for Real
		// must not force every caller's zero-wait path through an explicit
		// Advance(0) it would never make on Real.
		f.fireLocked(waiter, f.now)
	}
	return waiter
}

func (f *Fake) fireLocked(waiter *fakeWaiter, at time.Time) {
	waiter.channel <- at
	waiter.active = false
}

// Advance moves the fake clock forward by d, firing every sleep and After
// whose deadline falls at or before the new time, earliest deadline first
// (registration order breaks a tie).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		due := f.earliestDueLocked(target)
		if due == nil {
			break
		}
		f.now = due.due
		f.fireLocked(due, f.now)
	}
	f.now = target
}

func (f *Fake) earliestDueLocked(target time.Time) *fakeWaiter {
	var earliest *fakeWaiter
	for _, waiter := range f.waiters {
		if !waiter.active || waiter.due.After(target) {
			continue
		}
		if earliest == nil || waiter.due.Before(earliest.due) ||
			(waiter.due.Equal(earliest.due) && waiter.id < earliest.id) {
			earliest = waiter
		}
	}
	return earliest
}
