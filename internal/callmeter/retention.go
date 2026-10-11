package callmeter

import "time"

// SetRetention sets how long the store keeps its rows, from the settings file
// (internal/config): a report prunes every row older than d, and ledger ingest
// and transcript recovery bring none back from before it. 0, the default, keeps
// every row forever. Set it once, before the store is used.
func (s *Store) SetRetention(d time.Duration) { s.retention = d }

// Retention is the store's retention; 0 keeps every row forever.
func (s *Store) Retention() time.Duration { return s.retention }

// retentionFloor is the oldest moment a row may carry at now: the zero time
// when the store keeps every row, which bounds nothing.
func (s *Store) retentionFloor(now time.Time) time.Time {
	if s.retention <= 0 {
		return time.Time{}
	}
	return now.Add(-s.retention)
}
