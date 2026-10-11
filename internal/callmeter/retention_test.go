package callmeter

import (
	"testing"
	"time"
)

// TestRetentionFloor: a store keeps every row until SetRetention says
// otherwise, and its floor is then retention before now.
func TestRetentionFloor(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if store.Retention() != 0 || !store.retentionFloor(now).IsZero() {
		t.Fatalf("a new store has retention %v, floor %v; want 0 and the zero time: keep every row", store.Retention(), store.retentionFloor(now))
	}
	store.SetRetention(testRetention)
	if store.Retention() != testRetention {
		t.Errorf("Retention() = %v, want %v", store.Retention(), testRetention)
	}
	if got, want := store.retentionFloor(now), now.Add(-testRetention); !got.Equal(want) {
		t.Errorf("retentionFloor = %v, want %v", got, want)
	}
	store.SetRetention(0)
	if !store.retentionFloor(now).IsZero() {
		t.Errorf("retentionFloor after SetRetention(0) = %v, want the zero time", store.retentionFloor(now))
	}
}
