package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// detailNumber is the number the sanitized detail of event row e holds at the
// JSON key, NULL when the detail is not JSON, lacks the key or holds
// something else.
func detailNumber(key string) string {
	return `CASE WHEN json_valid(e.detail) THEN CASE WHEN json_type(e.detail, '$.` + key +
		`') IN ('integer', 'real') THEN json_extract(e.detail, '$.` + key + `') END END`
}

// detailBool is the boolean of the detail at the JSON key as 1 or 0, NULL when
// absent or not a boolean.
func detailBool(key string) string {
	return `CASE WHEN json_valid(e.detail) THEN CASE json_type(e.detail, '$.` + key +
		`') WHEN 'true' THEN 1 WHEN 'false' THEN 0 END END`
}

// Resumes lists one row per resumed session (a SessionStart whose source is
// resume), newest first: the chat, when, how long the session sat idle, its
// context size, whether the prompt cache likely expired and the cost Claude
// Code estimated for writing the cache again. A resume of an older Claude Code
// carries none of the cost fields: its cells are "-" and a note counts it,
// never 0.
func Resumes(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no resumes in window",
		Title:  f.title("resumes", n),
		Header: []string{"CHAT", "TIME", "IDLE", "CONTEXT", "CACHE EXPIRED", "EST CACHE WRITE USD"},
	}
	where, args := f.scoped(scope{ts: "e.ts", session: "e.session_id"})
	var sessions []*string
	var resumes, expired, bare int64
	var usd float64
	err := query(ctx, store, "resumes",
		`SELECT e.session_id, e.ts, `+detailNumber("seconds_since_last_response")+`, `+detailNumber("context_tokens")+`,
			`+detailBool("prompt_cache_likely_expired")+`, `+detailNumber("estimated_cache_write_usd")+`
		FROM events e WHERE e.event = ? AND e.source = 'resume' AND `+where+` ORDER BY e.ts DESC, e.event_id`,
		append([]any{callmeter.EventSessionStart}, args...),
		func(r rowSource) error {
			var session *string
			var ts *int64
			var idle, contextTokens, cost *float64
			var cacheExpired *int64
			if err := r.Scan(&session, &ts, &idle, &contextTokens, &cacheExpired, &cost); err != nil {
				return err
			}
			resumes++
			if cacheExpired != nil && *cacheExpired == 1 {
				expired++
			}
			if cost != nil {
				usd += *cost
			}
			if cacheExpired == nil && cost == nil {
				bare++
			}
			if len(t.Rows) >= f.limit() {
				return nil
			}
			idleCell, contextCell, expiredCell, costCell := "-", "-", "-", "-"
			sessions = append(sessions, session)
			if idle != nil {
				idleCell = compactDur(int64(*idle * 1000))
			}
			if contextTokens != nil {
				contextCell = itoa(int64(*contextTokens))
			}
			if cacheExpired != nil {
				expiredCell = map[int64]string{0: "no", 1: "yes"}[*cacheExpired]
			}
			if cost != nil {
				costCell = fmt.Sprintf("%.4f", *cost)
			}
			t.Rows = append(t.Rows, []string{"", stampCell(ts), idleCell, contextCell, expiredCell, costCell})
			return nil
		})
	if err != nil {
		return nil, err
	}
	nameChats(n, t, sessions)
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	if resumes > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf(
			"%d resumes, %d with the prompt cache likely expired, est. %.4f USD of cache writes", resumes, expired, usd))
	}
	if bare > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d resumes carry no cost fields (older Claude Code)", bare))
	}
	return t, nil
}
