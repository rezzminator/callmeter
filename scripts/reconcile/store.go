package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

// The store side, read from the snapshot over the columns
// internal/callmeter/store.go's schema names.

type sSession struct {
	id, model, startSource, transcriptPath string
	endReason                              string // sessions.end_reason; "": NULL
	firstTS                                int64
	lastTS                                 int64 // sessions.last_ts; 0: NULL
}

type sCall struct {
	id, session, agent, agentType, requestID, tool string
	ts                                             int64
	noTS                                           bool // ts IS NULL; ts reads 0
	hasSize                                        bool
	failed                                         int64 // -1: NULL
	// batchOnly: a delivered size with no error label; real-output tools
	// have no real size and no failure, including swept successes. Tools
	// with no separate real output have no landed outcome (failed NULL).
	batchOnly bool
	// noCommand: the stored input keeps only command_bytes, the command the
	// heredoc cutter could not cut safely (docs/design.md § Privacy).
	noCommand bool
	// delivered: bytes_delivered is set (PostToolBatch stored the size).
	delivered bool
}

func (c *sCall) resultLanded() bool {
	return c.hasSize || c.failed == 1 || (c.failed == 0 && !callmeter.HasRealOutput(c.tool))
}

type sRequest struct {
	id, session, agent, model, stopReason string
	ts                                    int64
	usage                                 tokens
	calls                                 int64
	pending                               int64
}

type sAgent struct {
	id, session, agentType, parent string
	started                        int64 // unix ms; 0: NULL
	toolUses                       int64 // -1: NULL
}

type sTurn struct {
	seq             int64
	ts              int64 // its stop, else its start (unix ms); 0: neither
	start           int64 // its start (unix ms); 0: none
	noStart, noStop bool
	rebuilt         bool // its stop is a SubagentStop recovery rebuilt from the transcript (rebuiltDetail)
}

// rebuiltDetail is the detail of a turn end recovery rebuilt from a transcript
// (callmeter.RecoveredDetail; a test pins the two).
const rebuiltDetail = `{"from_transcript":true}`

type sEvent struct {
	session, event, agent, source, promptID string
	ts                                      int64
	stopHookActive                          bool // a Stop's turns row: stop_hook_active true
}

type sFault struct {
	session, toolUseID, stage, err string
	ts                             int64
}

type storeData struct {
	sessions     map[string]*sSession
	calls        map[string]*sCall
	requests     map[string]*sRequest
	agents       map[string]*sAgent
	turns        map[string][]sTurn
	turnSessions map[string]bool
	events       []sEvent
	parts        map[string][]sPart
	faults       []sFault
	maxParser    int64
	latest       int64
	earliest     int64 // the earliest session's first row; 0: no session
}

type sPart struct {
	lang   string // command_parts.lang: sh, python, …
	status string
	parser int64
}

func loadStore(ctx context.Context, path string) (*storeData, error) {
	db, err := sqlitedb.OpenReadWrite(path, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return nil, fmt.Errorf("snapshot %s: set query_only: %w", path, err)
	}
	d := &storeData{
		sessions: map[string]*sSession{}, calls: map[string]*sCall{}, requests: map[string]*sRequest{},
		agents: map[string]*sAgent{}, turns: map[string][]sTurn{}, parts: map[string][]sPart{},
		turnSessions: map[string]bool{},
	}
	steps := []struct {
		name, query string
		scan        func(*sql.Rows) error
	}{
		{"sessions", `SELECT session_id, COALESCE(first_ts,0), COALESCE(model,''), COALESCE(start_source,''), COALESCE(transcript_path,''), COALESCE(end_reason,''), COALESCE(last_ts,0) FROM sessions`,
			func(r *sql.Rows) error {
				s := &sSession{}
				if err := r.Scan(&s.id, &s.firstTS, &s.model, &s.startSource, &s.transcriptPath, &s.endReason, &s.lastTS); err != nil {
					return err
				}
				d.sessions[s.id] = s
				if s.firstTS > 0 && (d.earliest == 0 || s.firstTS < d.earliest) {
					d.earliest = s.firstTS
				}
				return nil
			}},
		{"calls", `SELECT tool_use_id, COALESCE(session_id,''), COALESCE(agent_id,''), COALESCE(agent_type,''), COALESCE(request_id,''), COALESCE(ts,0), ts IS NULL, COALESCE(tool,''), bytes_real IS NOT NULL, COALESCE(failed,-1), error IS NULL, COALESCE(json_valid(input) AND json_type(input,'$.command') IS NULL AND json_type(input,'$.command_bytes') IS NOT NULL, 0), bytes_delivered IS NOT NULL FROM calls`,
			func(r *sql.Rows) error {
				c := &sCall{}
				var noError bool
				if err := r.Scan(&c.id, &c.session, &c.agent, &c.agentType, &c.requestID, &c.ts, &c.noTS, &c.tool, &c.hasSize, &c.failed, &noError, &c.noCommand, &c.delivered); err != nil {
					return err
				}
				if callmeter.HasRealOutput(c.tool) {
					c.batchOnly = c.delivered && noError && !c.hasSize && c.failed != 1
				} else {
					c.batchOnly = c.delivered && noError && c.failed == -1
				}
				d.calls[c.id] = c
				d.latest = max(d.latest, c.ts)
				return nil
			}},
		{"requests", `SELECT request_id, COALESCE(session_id,''), COALESCE(agent_id,''), COALESCE(ts,0), COALESCE(model,''), COALESCE(stop_reason,''), COALESCE(input_tokens,-1), COALESCE(output_tokens,-1), COALESCE(cache_read_tokens,-1), COALESCE(cache_creation_tokens,-1), COALESCE(calls,-1), COALESCE(pending,0) FROM requests`,
			func(r *sql.Rows) error {
				q := &sRequest{}
				if err := r.Scan(&q.id, &q.session, &q.agent, &q.ts, &q.model, &q.stopReason, &q.usage.input, &q.usage.output, &q.usage.cacheRead, &q.usage.cacheCreation, &q.calls, &q.pending); err != nil {
					return err
				}
				d.requests[q.id] = q
				d.latest = max(d.latest, q.ts)
				return nil
			}},
		{"agents", `SELECT agent_id, COALESCE(session_id,''), COALESCE(agent_type,''), COALESCE(parent_tool_use_id,''), COALESCE(tool_uses,-1), COALESCE(started,0) FROM agents`,
			func(r *sql.Rows) error {
				a := &sAgent{}
				if err := r.Scan(&a.id, &a.session, &a.agentType, &a.parent, &a.toolUses, &a.started); err != nil {
					return err
				}
				d.agents[a.id] = a
				return nil
			}},
		{"agent_turns", `SELECT t.agent_id, t.seq, t.started IS NULL, t.stopped IS NULL, COALESCE(t.stopped, t.started, 0), COALESCE(t.started, 0),
			COALESCE(e.event = 'SubagentStop' AND e.detail = '` + rebuiltDetail + `', 0) FROM agent_turns t LEFT JOIN events e ON e.event_id = t.stop_event_id ORDER BY t.agent_id, t.seq`,
			func(r *sql.Rows) error {
				var id string
				var t sTurn
				if err := r.Scan(&id, &t.seq, &t.noStart, &t.noStop, &t.ts, &t.start, &t.rebuilt); err != nil {
					return err
				}
				d.turns[id] = append(d.turns[id], t)
				return nil
			}},
		{"turns", `SELECT DISTINCT session_id FROM turns WHERE session_id IS NOT NULL AND session_id != ''`,
			func(r *sql.Rows) error {
				var id string
				if err := r.Scan(&id); err != nil {
					return err
				}
				d.turnSessions[id] = true
				return nil
			}},
		// A Stop's stop_hook_active lives on its turns row, keyed by the same event_id.
		{"events", `SELECT COALESCE(e.session_id,''), COALESCE(e.event,''), COALESCE(e.agent_id,''), COALESCE(e.source,''), COALESCE(e.prompt_id,''), COALESCE(e.ts,0), COALESCE(t.stop_hook_active,0) = 1 FROM events e LEFT JOIN turns t ON t.event_id = e.event_id`,
			func(r *sql.Rows) error {
				var e sEvent
				if err := r.Scan(&e.session, &e.event, &e.agent, &e.source, &e.promptID, &e.ts, &e.stopHookActive); err != nil {
					return err
				}
				d.events = append(d.events, e)
				d.latest = max(d.latest, e.ts)
				return nil
			}},
		{"command_parts", `SELECT tool_use_id, COALESCE(parse_status,''), parser, COALESCE(lang,'') FROM command_parts`,
			func(r *sql.Rows) error {
				var id string
				var p sPart
				if err := r.Scan(&id, &p.status, &p.parser, &p.lang); err != nil {
					return err
				}
				d.parts[id] = append(d.parts[id], p)
				d.maxParser = max(d.maxParser, p.parser)
				return nil
			}},
		{"faults", `SELECT COALESCE(session_id,''), COALESCE(tool_use_id,''), COALESCE(stage,''), COALESCE(ts,0), COALESCE(error,'') FROM faults`,
			func(r *sql.Rows) error {
				var f sFault
				if err := r.Scan(&f.session, &f.toolUseID, &f.stage, &f.ts, &f.err); err != nil {
					return err
				}
				d.faults = append(d.faults, f)
				return nil
			}},
	}
	for _, s := range steps {
		if err := queryEach(ctx, db, s.query, s.scan); err != nil {
			return nil, fmt.Errorf("snapshot %s: read %s: %w", path, s.name, err)
		}
	}
	return d, nil
}

func queryEach(ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) error) (err error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// scratchURI escapes what SQLite's URI parser would read as syntax.
var scratchURI = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// loadScratch indexes the scratch stores under root: every callmeter.db but
// the snapshot itself, opened read-only, mapping each session_id it records to
// its path (the first in walk order). A store or directory that cannot be read
// is listed, never skipped silently; an absent root holds none.
func loadScratch(ctx context.Context, root, snapshot string) (map[string]string, int, []string, error) {
	sessions := map[string]string{}
	if root == "" {
		return sessions, 0, nil, nil
	}
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return sessions, 0, nil, nil
	} else if err != nil {
		return nil, 0, nil, fmt.Errorf("--scratch %s: %w", root, err)
	}
	snap, err := filepath.Abs(snapshot)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("resolve --db %s: %w", snapshot, err)
	}
	read := 0
	var unreadable []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Name() != "callmeter.db" {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", path, err))
			return nil
		}
		if abs == snap {
			return nil
		}
		if err := refuseLiveStore(abs); err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", path, err))
			return nil
		}
		ids, err := scratchSessions(ctx, abs)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", path, err))
			return nil
		}
		read++
		for _, id := range ids {
			if _, ok := sessions[id]; !ok {
				sessions[id] = path
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, 0, nil, fmt.Errorf("walk --scratch %s: %w", root, walkErr)
	}
	return sessions, read, unreadable, nil
}

// scratchSessions lists sessions recorded by a sessions row or a fault naming
// them, without writing to the scratch store: read-only beside its -wal, else
// immutable (no WAL to miss, and no -shm to create).
func scratchSessions(ctx context.Context, path string) (ids []string, err error) {
	query := "mode=ro&immutable=1"
	if _, statErr := os.Stat(path + "-wal"); statErr == nil {
		query = "mode=ro"
	}
	db, err := sql.Open("sqlite", "file:"+scratchURI.Replace(path)+"?"+query)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close: %w", cerr)
		}
	}()
	err = queryEach(ctx, db, `SELECT session_id FROM sessions UNION SELECT session_id FROM faults WHERE session_id IS NOT NULL AND session_id <> ''`, func(r *sql.Rows) error {
		var id string
		if err := r.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	})
	return ids, err
}
