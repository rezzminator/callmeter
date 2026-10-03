// Package report answers the callmeter report topics over the store
// (docs/design/hooks/callmeter.md § Reports): the shared filter, the prune
// that runs before every report, the command-parse cache fill, the
// fixed-width table and one query per topic.
package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Retention is the store's window: rows older than this are pruned before
// every report, and it is the default --since.
const Retention = callmeter.Retention

// DefaultLimit is the rows per table when Filter.Limit is not positive.
const DefaultLimit = 25

// EmptyLine is the default body line of a table with no rows.
const EmptyLine = "callmeter: no calls recorded in window"

// Filter narrows every topic. A zero Since covers the whole retention window
// (PruneExpired has pruned everything older); an empty field does not filter.
type Filter struct {
	Since     time.Time
	Project   string // calls whose cwd is Project or under it
	AgentType string // calls made by that agent type
	Session   string // calls in that session
	Limit     int    // rows per table; <= 0 is DefaultLimit
	// OwnSeat is the Claude Code config dir the report itself runs under;
	// the coverage topic scans its projects/ beside every seat the store
	// recorded. Empty when it could not be resolved.
	OwnSeat string
}

// NameOf resolves a session id to its chat name; the CLI reads the title
// entries of the session's transcript, read-only.
type NameOf func(sessionID string) (string, error)

var durationSince = regexp.MustCompile(`^(\d+)([dh])$`)

// ParseSince reads a --since value: a duration in days or hours ("7d",
// "24h") counted back from now, or a UTC date ("2026-09-01"). An empty value
// is the whole retention window, now - Retention.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return now.Add(-Retention), nil
	}
	if m := durationSince.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("callmeter report: --since %q: %w", s, err)
		}
		unit := time.Hour
		if m[2] == "d" {
			unit = 24 * time.Hour
		}
		return now.Add(-time.Duration(n) * unit), nil
	}
	day, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"callmeter report: --since %q is neither a duration (7d, 24h) nor a date (2026-09-01)",
			s,
		)
	}
	return day.UTC(), nil
}

// PruneExpired prunes every row older than the retention window before a report
// runs, and returns how many rows went.
func PruneExpired(ctx context.Context, store *callmeter.Store, now time.Time) (int64, error) {
	removed, err := store.Prune(ctx, now.Add(-Retention))
	if err != nil {
		return 0, fmt.Errorf("callmeter report: prune before report: %w", err)
	}
	return removed, nil
}

// Table is one topic's answer.
type Table struct {
	Title  string // the heading: topic, filters, window
	Header []string
	Rows   [][]string
	Notes  []string // one line per named gap
	Empty  string   // the body line when Rows is empty; "" is EmptyLine
}

// Render writes the heading, the fixed-width rows (or EmptyLine) and the
// note lines.
func (t *Table) Render(w io.Writer) error {
	if _, err := fmt.Fprintln(w, t.Title); err != nil {
		return fmt.Errorf("callmeter report: write heading: %w", err)
	}
	if len(t.Rows) == 0 {
		empty := t.Empty
		if empty == "" {
			empty = EmptyLine
		}
		if _, err := fmt.Fprintln(w, empty); err != nil {
			return fmt.Errorf("callmeter report: write empty line: %w", err)
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		lines := append([][]string{t.Header}, t.Rows...)
		for _, cells := range lines {
			if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
				return fmt.Errorf("callmeter report: write row: %w", err)
			}
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("callmeter report: flush table: %w", err)
		}
	}
	for _, note := range t.Notes {
		if _, err := fmt.Fprintln(w, "note: "+note); err != nil {
			return fmt.Errorf("callmeter report: write note: %w", err)
		}
	}
	return nil
}

// jsonTable is the --json object of a store that is there.
type jsonTable struct {
	Topic   string     `json:"topic"`
	Store   string     `json:"store"`
	Title   string     `json:"title"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Notes   []string   `json:"notes"`
}

// RenderJSON writes the table as the one --json object: the topic, `store`
// "present", the title, the header cells as columns, the rows as arrays of the
// same cell strings and the notes without their `note:` prefix. An empty
// window is `"rows": []`, never null.
func (t *Table) RenderJSON(w io.Writer, topic string) error {
	out := jsonTable{
		Topic: topic, Store: "present", Title: t.Title,
		Columns: nonNil(t.Header), Rows: t.Rows, Notes: nonNil(t.Notes),
	}
	if out.Rows == nil {
		out.Rows = [][]string{}
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(out); err != nil {
		return fmt.Errorf("callmeter report: write %s json: %w", topic, err)
	}
	return nil
}

func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

func (f Filter) limit() int {
	if f.Limit <= 0 {
		return DefaultLimit
	}
	return f.Limit
}

// title names the topic, the active filters and the window.
func (f Filter) title(topic string, names *names) string {
	parts := []string{"callmeter " + topic}
	parts = append(parts, "window: since "+f.Since.UTC().Format("2006-01-02 15:04")+" UTC")
	if f.Project != "" {
		parts = append(parts, "project="+f.Project)
	}
	if f.AgentType != "" {
		parts = append(parts, "agent-type="+f.AgentType)
	}
	if f.Session != "" {
		parts = append(parts, fmt.Sprintf("session=%s (%s)", f.Session, names.of(f.Session)))
	}
	parts = append(parts, fmt.Sprintf("limit=%d", f.limit()))
	return strings.Join(parts, " · ")
}

// where is the calls filter over alias c, as an SQL condition and its args.
func (f Filter) where() (string, []any) {
	conds := []string{"1=1"}
	var args []any
	col := func(name string) string { return "c." + name }
	if !f.Since.IsZero() {
		conds = append(conds, col("ts")+" >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Project != "" {
		cond, projectArgs := underProject(col("cwd"), f.Project)
		conds = append(conds, cond)
		args = append(args, projectArgs...)
	}
	if f.AgentType != "" {
		conds = append(conds, col("agent_type")+" = ?")
		args = append(args, f.AgentType)
	}
	if f.Session != "" {
		conds = append(conds, col("session_id")+" = ?")
		args = append(args, f.Session)
	}
	return strings.Join(conds, " AND "), args
}

// underProject is the condition that column col is project or a path under it.
// substr counts characters, so the prefix length is in runes.
func underProject(col, project string) (string, []any) {
	project = filepath.Clean(project)
	under := strings.TrimSuffix(project, "/") + "/"
	return fmt.Sprintf("(%s = ? OR substr(%s, 1, ?) = ?)", col, col), []any{project, utf8.RuneCountInString(under), under}
}

// scope names the columns of a table that carry the Filter's fields; an empty
// name is a column the table lacks, so that flag does not narrow it.
type scope struct {
	ts        string // the time column or expression
	session   string // the session id column
	agentType string // the agent type column or expression
}

// scoped is the condition over a table that is not calls, as an SQL condition
// and its args. A table with no cwd takes --project through its session: the
// sessions whose own cwd, or whose calls' cwd, is under the project.
func (f Filter) scoped(s scope) (string, []any) {
	conds := []string{"1=1"}
	var args []any
	if !f.Since.IsZero() && s.ts != "" {
		conds = append(conds, s.ts+" >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Project != "" && s.session != "" {
		cond, projectArgs := underProject("cwd", f.Project)
		conds = append(conds, s.session+" IN (SELECT session_id FROM sessions WHERE "+cond+
			" UNION SELECT session_id FROM calls WHERE "+cond+")")
		args = append(args, projectArgs...)
		args = append(args, projectArgs...)
	}
	if f.AgentType != "" && s.agentType != "" {
		conds = append(conds, s.agentType+" = ?")
		args = append(args, f.AgentType)
	}
	if f.Session != "" && s.session != "" {
		conds = append(conds, s.session+" = ?")
		args = append(args, f.Session)
	}
	return strings.Join(conds, " AND "), args
}

// inapplicable lists, per topic, the flags the topic has no column for.
var inapplicable = map[string][]string{
	"faults":   {"project", "agent-type"},
	"sessions": {"agent-type"},
	"coverage": {"project", "agent-type"},
}

// InapplicableNotes names every flag set on the command line that topic
// cannot apply, one `--{flag} does not apply to {topic}` each, so an unnarrowed
// table is never read as a narrowed one.
func InapplicableNotes(topic string, f Filter) []string {
	var notes []string
	for _, flag := range inapplicable[topic] {
		if (flag == "project" && f.Project != "") || (flag == "agent-type" && f.AgentType != "") {
			notes = append(notes, fmt.Sprintf("--%s does not apply to %s", flag, topic))
		}
	}
	return notes
}

// stampCell is a Unix-ms time as the tables print it, UTC; NULL is "-".
func stampCell(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return time.UnixMilli(*ms).UTC().Format("2006-01-02 15:04")
}

// textCell is a nullable string cell; NULL and "" are "-".
func textCell(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// intCell is a nullable integer cell; NULL is "-".
func intCell(n *int64) string {
	if n == nil {
		return "-"
	}
	return itoa(*n)
}

// names caches chat names; lookup failures become a count and safe label note.
type names struct {
	fn             NameOf
	cache          map[string]string
	err            error
	failedSessions int
}

func newNames(fn NameOf) *names { return &names{fn: fn, cache: map[string]string{}} }

func (n *names) of(session string) string {
	if session == "" {
		return "?"
	}
	if name, ok := n.cache[session]; ok {
		return name
	}
	name := "?"
	if n.fn == nil {
		n.failedSessions++
		if n.err == nil {
			n.err = fmt.Errorf("no chat-name source was given")
		}
	} else if got, err := n.fn(session); err != nil {
		n.failedSessions++
		if n.err == nil {
			n.err = fmt.Errorf("session %s: %w", session, err)
		}
	} else if got != "" {
		name = got
	}
	n.cache[session] = name
	return name
}

func (n *names) notes() []string {
	if n.err == nil {
		return nil
	}
	label := "unreadable"
	switch {
	case errors.Is(n.err, fs.ErrPermission):
		label = "permission denied"
	case errors.Is(n.err, fs.ErrNotExist):
		label = "not found"
	}
	return []string{fmt.Sprintf("chat names could not be read for %d sessions (first: %s)", n.failedSessions, label)}
}

// row scanning without naming database/sql: the store's rows satisfy this.
type rowSource interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// query runs statement and hands each row to scan.
func query(
	ctx context.Context,
	store *callmeter.Store,
	what, statement string,
	args []any,
	scan func(rowSource) error,
) error {
	rows, err := store.DB().QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("callmeter report: query %s: %w", what, err)
	}
	return readRows(rows, what, scan)
}

func readRows(rows rowSource, what string, scan func(rowSource) error) error {
	for rows.Next() {
		if err := scan(rows); err != nil {
			closeErr := rows.Close()
			if closeErr != nil {
				return fmt.Errorf("callmeter report: read %s: %w (close: %v)", what, err, closeErr)
			}
			return fmt.Errorf("callmeter report: read %s: %w", what, err)
		}
	}
	if err := rows.Err(); err != nil {
		closeErr := rows.Close()
		if closeErr != nil {
			return fmt.Errorf("callmeter report: read %s: %w (close: %v)", what, err, closeErr)
		}
		return fmt.Errorf("callmeter report: read %s: %w", what, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("callmeter report: close %s rows: %w", what, err)
	}
	return nil
}

// toolEvents are the hook events that record a call: a payload or store fault
// of one names a lost call even when it carries no tool_use_id.
var toolEvents = []string{"PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch"}

// unsettledMarkers are the tails of the transcript faults recovery writes for a
// turn or agent turn it read in full and could not settle (callmeter's
// UnfilledTurnEnd, UnfilledAgentStop, UnfilledAgentTurn, UnfilledStopReply):
// no tool_use_id, no call. UnfilledCall is not among them: it names its call,
// whose row exists.
var unsettledMarkers = []string{
	callmeter.UnfilledTurnEnd, callmeter.UnfilledAgentStop, callmeter.UnfilledAgentTurn, callmeter.UnfilledStopReply,
}

// idlessLostCall is the SQL condition that the id-less fault row aliased t
// names a lost call: a payload or store fault whose error opens with a tool
// event's name, bare or quoted, as every such fault hookentry writes does.
func idlessLostCall(t string) string {
	var names []string
	for _, event := range toolEvents {
		names = append(names, fmt.Sprintf(`COALESCE(%[1]s.error, '') GLOB '%[2]s *' OR COALESCE(%[1]s.error, '') GLOB '"%[2]s" *'`, t, event))
	}
	return fmt.Sprintf(`(%[1]s.stage IN ('%[2]s', '%[3]s') AND (%[4]s) AND COALESCE(%[1]s.error, '') <> '%[5]s')`,
		t, callmeter.StagePayload, callmeter.StageStore, strings.Join(names, " OR "), callmeter.BatchWithoutCalls)
}

// unsettledMarker is the SQL condition that the id-less fault row aliased t is
// one of recovery's unsettledMarkers: a transcript fault ending in its text.
func unsettledMarker(t string) string {
	var tails []string
	for _, marker := range unsettledMarkers {
		tails = append(tails, fmt.Sprintf(`substr(COALESCE(%s.error, ''), -%d) = '%s'`,
			t, len(marker), strings.ReplaceAll(marker, "'", "''")))
	}
	return fmt.Sprintf(`(%s.stage = '%s' AND (%s))`, t, callmeter.StageTranscript, strings.Join(tails, " OR "))
}

// recordingNotes names what the hook never recorded: calls (payload, store or
// transcript faults), turns recovery could not settle, the other faults naming
// no call, and events (a binary the wrapper could not run, a hook a signal, a
// busy store or an unavailable one ended before it recorded). A call counts
// once however many faults name it, and not at all when its row was written
// anyway; a fault with no tool_use_id counts as one lost call only when it is a
// tool event's payload or store fault (idlessLostCall), else in the
// unsettled-turns note (unsettledMarker) or the note of faults naming no call.
func recordingNotes(ctx context.Context, store *callmeter.Store, f Filter) ([]string, error) {
	var notes []string
	faultWhere, faultArgs := faultFilter(f, "faults")
	var unrecorded, unsettled, other sql.NullInt64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT CASE WHEN COALESCE(tool_use_id, '') <> ''
			AND NOT EXISTS (SELECT 1 FROM calls WHERE calls.tool_use_id = faults.tool_use_id) THEN tool_use_id END)
			+ SUM(COALESCE(tool_use_id, '') = '' AND `+idlessLostCall("faults")+`),
			SUM(COALESCE(tool_use_id, '') = '' AND `+unsettledMarker("faults")+`),
			SUM(COALESCE(tool_use_id, '') = '' AND NOT `+idlessLostCall("faults")+` AND NOT `+unsettledMarker("faults")+`)
		FROM faults WHERE stage IN (?, ?, ?) AND `+faultWhere,
		append([]any{callmeter.StagePayload, callmeter.StageStore, callmeter.StageTranscript}, faultArgs...)...,
	).Scan(&unrecorded, &unsettled, &other); err != nil {
		return nil, fmt.Errorf("callmeter report: count unrecorded calls: %w", err)
	}
	if unrecorded.Int64 > 0 {
		notes = append(
			notes,
			fmt.Sprintf(
				"%d calls not recorded (payload, store or transcript faults; see the faults topic)",
				unrecorded.Int64,
			),
		)
	}
	if unsettled.Int64 > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d turns or agent turns recovery could not settle from transcripts; see the faults topic", unsettled.Int64))
	}
	if other.Int64 > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d other payload, store or transcript faults, naming no call (see the faults topic)", other.Int64))
	}
	for _, lost := range []struct{ stage, why string }{
		{callmeter.StageBinary, "binary unavailable"},
		{callmeter.StageTerminated, "hook terminated before recording"},
	} {
		var missed int64
		if err := store.DB().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM faults WHERE stage = ? AND "+faultWhere,
			append([]any{lost.stage}, faultArgs...)...,
		).Scan(&missed); err != nil {
			return nil, fmt.Errorf("callmeter report: count unrecorded %s events: %w", lost.stage, err)
		}
		if missed > 0 {
			notes = append(notes, fmt.Sprintf("%d events unrecorded: %s", missed, lost.why))
		}
	}
	return notes, nil
}

// pendingNotes names the requests whose context size is not yet read, each
// by why: its transcript faulted, its session ended before the transcript held
// it, or its session has not ended (still live, or killed before its Stop).
func pendingNotes(ctx context.Context, store *callmeter.Store, f Filter) ([]string, error) {
	var notes []string
	reqWhere, reqArgs := requestFilter(f)
	args := append([]any{callmeter.StageTranscript, callmeter.PendingPrefix, callmeter.EventSessionEnd}, reqArgs...)
	err := query(ctx, store, "pending requests",
		`SELECT CASE
			WHEN EXISTS (SELECT 1 FROM faults ft WHERE ft.stage = ? AND (
				ft.tool_use_id IN (SELECT c.tool_use_id FROM calls c WHERE c.request_id = r.request_id)
				OR ? || ft.tool_use_id = r.request_id))
				THEN 'its transcript could not be read'
			WHEN EXISTS (SELECT 1 FROM events e WHERE e.session_id = r.session_id AND e.event = ? AND e.ts >= r.ts)
				THEN 'its session ended before the transcript held it'
			ELSE 'its session has not ended: still live, or killed before its Stop'
		END reason, COUNT(*)
		FROM requests r WHERE r.pending = 1 AND `+reqWhere+`
		GROUP BY reason ORDER BY reason`,
		args,
		func(row rowSource) error {
			var reason string
			var n int64
			if err := row.Scan(&reason, &n); err != nil {
				return err
			}
			notes = append(notes,
				fmt.Sprintf("%d requests still pending (%s): context size unknown, not counted", n, reason))
			return nil
		})
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// topicNotes is the gap notes of a topic over the lifecycle tables: what was
// never recorded, requests still pending when the topic reads their tokens,
// and the chat names that could not be read.
func topicNotes(ctx context.Context, store *callmeter.Store, f Filter, n *names, withPending bool) ([]string, error) {
	notes, err := recordingNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	if withPending {
		pending, err := pendingNotes(ctx, store, f)
		if err != nil {
			return nil, err
		}
		notes = append(notes, pending...)
	}
	return append(notes, n.notes()...), nil
}

// batchOnlyCall is a call only PostToolBatch wrote: a delivered size and none of
// the outcome a PostToolUse, PostToolUseFailure or a refusal label sets.
const batchOnlyCall = "(c.bytes_delivered IS NOT NULL AND c.failed IS NULL AND c.error IS NULL)"

// gapNotes names every gap the window holds: calls not recorded, events the
// binary never delivered, requests still pending, calls with no delivered size,
// calls PostToolBatch alone recorded, snippets unparsed per status and Bash
// calls never parsed.
func gapNotes(ctx context.Context, store *callmeter.Store, f Filter) ([]string, error) {
	notes, err := recordingNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	pending, err := pendingNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	notes = append(notes, pending...)
	// A call stored before every hook set a ts has none, so --since never
	// reaches it: it is named whether or not the report is windowed. A row
	// only PostToolBatch wrote is named by its own note below instead.
	unwindowed := f
	unwindowed.Since = time.Time{}
	noTSWhere, noTSArgs := unwindowed.where()
	var noTS int64
	if err := store.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM calls c WHERE c.ts IS NULL AND NOT "+batchOnlyCall+" AND "+noTSWhere, noTSArgs...,
	).Scan(&noTS); err != nil {
		return nil, fmt.Errorf("callmeter report: count calls without a ts: %w", err)
	}
	if noTS > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d calls have no ts (stored before every hook set one): outside any --since window", noTS))
	}
	where, args := f.where()
	// A call with no delivered size is unknown, never 0 bytes: each is named by
	// why it has none, and a denied, rejected or refused call by its outcome
	// label. The IN lists are sized from outcomes, so a label added here needs
	// no SQL edit.
	outcomes := []any{callmeter.OutcomeDeniedByHook, callmeter.OutcomeDeniedByPermission,
		callmeter.OutcomeRejectedByUser, callmeter.OutcomeRefused}
	in := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(outcomes)), ", ") + ")"
	err = query(ctx, store, "calls without a delivered size",
		`SELECT CASE
			WHEN c.error IN `+in+` THEN c.error
			WHEN c.failed = 1 THEN 'failed, no PostToolBatch recorded'
			WHEN c.failed = 0 THEN 'ran, no PostToolBatch recorded: its turn was abandoned or is still running'
			WHEN c.agent_id IS NOT NULL AND c.agent_type IS NULL THEN 'a Claude Code internal agent''s, no transcript'
			ELSE 'only PreToolUse recorded: interrupted, killed or still running'
		END, COALESCE(c.error IN `+in+`, 0), c.bytes_delivered IS NULL, COUNT(*)
		FROM calls c WHERE (c.bytes_delivered IS NULL OR c.error IN `+in+`) AND `+where+`
		GROUP BY 1, 2, 3 ORDER BY 2 DESC, 1, 3`,
		append(append(append(append([]any{}, outcomes...), outcomes...), outcomes...), args...),
		func(r rowSource) error {
			var class string
			var outcome, unknown bool
			var n int64
			if err := r.Scan(&class, &outcome, &unknown, &n); err != nil {
				return err
			}
			switch {
			case outcome && unknown:
				notes = append(notes, fmt.Sprintf("%d calls %s: size unknown, not counted", n, class))
			case outcome:
				notes = append(notes, fmt.Sprintf("%d calls %s", n, class))
			default:
				notes = append(notes,
					fmt.Sprintf("%d calls have no delivered size (%s): size unknown, not counted", n, class))
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	// A call only PostToolBatch wrote has its delivered size and nothing the
	// PostToolUse would have set: named by its tool, since a tool a
	// function-hooks plugin answers in its own tool.call hook fires no
	// PostToolUse on any call (mcp__sub-agent-compact__compact). A row from
	// before the batch set a ts is named in any window, as the no-ts note's are.
	err = query(ctx, store, "calls recorded by PostToolBatch alone",
		`SELECT COALESCE(c.tool, '(none)'), COUNT(*) FROM calls c
		WHERE `+batchOnlyCall+` AND ((c.ts IS NULL AND `+noTSWhere+`) OR (c.ts IS NOT NULL AND `+where+`))
		GROUP BY 1 ORDER BY 1`,
		append(append([]any{}, noTSArgs...), args...),
		func(r rowSource) error {
			var tool string
			var n int64
			if err := r.Scan(&tool, &n); err != nil {
				return err
			}
			notes = append(notes, fmt.Sprintf("%d calls of %s have only their PostToolBatch size (no PostToolUse "+
				"or PostToolUseFailure landed: a tool a function-hooks plugin answers itself fires neither, "+
				"else the hook was cancelled or failed): real size, duration and outcome unknown, not counted", n, tool))
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = query(ctx, store, "unparsed snippets",
		`SELECT p.parse_status, COUNT(*) FROM command_parts p JOIN calls c ON c.tool_use_id = p.tool_use_id
		WHERE COALESCE(p.parse_status, '') != ? AND `+where+` GROUP BY p.parse_status ORDER BY p.parse_status`,
		append([]any{statusOK}, args...),
		func(r rowSource) error {
			var status *string
			var n int64
			if err := r.Scan(&status, &n); err != nil {
				return err
			}
			label := "(none)"
			if status != nil {
				label = *status
			}
			notes = append(notes, fmt.Sprintf("%d snippets unparsed: %s", n, label))
			return nil
		})
	if err != nil {
		return nil, err
	}
	var unparsedCalls int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calls c WHERE c.tool = 'Bash'
		AND NOT EXISTS (SELECT 1 FROM command_parts p WHERE p.tool_use_id = c.tool_use_id) AND `+where, args...,
	).Scan(&unparsedCalls); err != nil {
		return nil, fmt.Errorf("callmeter report: count unparsed Bash calls: %w", err)
	}
	if unparsedCalls > 0 {
		notes = append(
			notes,
			fmt.Sprintf("%d Bash calls skipped by the parser (relative or missing cwd, or no command)", unparsedCalls),
		)
	}
	return notes, nil
}

// faultFilter narrows faults by window and session only: a fault that failed
// to record its call has no calls row to carry the other filters.
func faultFilter(f Filter, table string) (string, []any) {
	conds := []string{"1=1"}
	var args []any
	if !f.Since.IsZero() {
		conds = append(conds, table+".ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Session != "" {
		conds = append(conds, table+".session_id = ?")
		args = append(args, f.Session)
	}
	return strings.Join(conds, " AND "), args
}

// requestFilter narrows requests by window and session.
func requestFilter(f Filter) (string, []any) { return faultFilter(f, "r") }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// OlderRulesNotes is the note every topic ends with while the store holds
// calls written under older privacy rules (Store.UnredactedCalls, over the
// whole store, not the window): their count and the command that rewrites
// them, never their text. None when every call is in the current form.
func OlderRulesNotes(ctx context.Context, store *callmeter.Store) ([]string, error) {
	n, err := store.UnredactedCalls(ctx)
	if err != nil || n == 0 {
		return nil, err
	}
	return []string{fmt.Sprintf("%d calls hold text stored under older privacy rules; run `callmeter redact` to scrub them", n)}, nil
}
