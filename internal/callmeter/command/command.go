// Package command owns `callmeter report`: the report action over the call
// store, its flags and the chat-name lookup; cmd/callmeter only hands it argv
// and the environment.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/report"
	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/config"
	"github.com/rezzminator/callmeter/internal/faultlog"
	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/pluginlog"
)

// Usage is the report action's usage text.
const Usage = `usage: callmeter report {files|writes|commands|context|sequences|faults|sessions|prompts|effort|tokens|agents|outcomes|coverage|events|compactions|cost|hooks|turns|resumes|waiting|cache} [--since D] [--project P]
                       [--agent-type T] [--session S] [--limit N] [--view V] [--json]
  --since D         a duration (7d, 24h) or a date (2026-09-01); default: the whole store, or with retention_days set in config.json its retention window, which also floors it
  --view V          cost only: chat (default), agent or call
  --json            one JSON object on stdout instead of the text table`

type topicFunc func(context.Context, *callmeter.Store, report.Filter, report.NameOf) (*report.Table, error)

var topics = map[string]topicFunc{
	"files": report.Files, "writes": report.Writes, "commands": report.Commands,
	"context": report.Context, "sequences": report.Sequences, "faults": report.Faults,
	"sessions": report.Sessions, "prompts": report.Prompts, "effort": report.Effort, "tokens": report.Tokens,
	"agents": report.Agents, "outcomes": report.Outcomes, "coverage": report.Coverage, "events": report.Events,
	"compactions": report.Compactions, "cost": report.Cost, "hooks": report.Hooks, "turns": report.Turns,
	"resumes": report.Resumes, "waiting": report.Waiting, "cache": report.Cache,
}

// CLI is `callmeter {args}` for the report action: the reports over the call
// store the hook fills, with chat names read from the transcripts. args starts
// with "report". It returns the process exit code: 0 ok, 1 a store, parse or
// report error, 2 usage.
func CLI(args []string, stdout, stderr io.Writer, getenv paths.Getenv) int {
	if len(args) > 0 && args[0] == "report" {
		return reportAction(context.Background(), args[1:], stdout, stderr, getenv)
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "callmeter: unknown action %q\n", args[0])
	}
	fmt.Fprintln(stderr, Usage)
	pluginLog(stderr, getenv, "info", "usage-refused", "internal/callmeter/command/command.go#CLI", "unknown or missing action")
	return 2
}

type flagValues struct {
	since, project, agentType, session string
	limit                              int
	view                               string
	json                               bool
}

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *flagValues) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, Usage) }
	values := &flagValues{}
	flags.StringVar(&values.since, "since", "", "a duration (7d, 24h) or a date (2026-09-01)")
	flags.StringVar(&values.project, "project", "", "calls whose cwd is this dir or under it")
	flags.StringVar(&values.agentType, "agent-type", "", "calls made by this agent type")
	flags.StringVar(&values.session, "session", "", "calls in this session")
	flags.IntVar(&values.limit, "limit", report.DefaultLimit, "rows per table")
	flags.StringVar(&values.view, "view", "", "cost only: chat (default), agent or call")
	flags.BoolVar(&values.json, "json", false, "one JSON object on stdout instead of the text table")
	return flags, values
}

// parseFlagsAnywhere reads flags before and after the positional words:
// parse, take the first word, parse the rest. A bare `--` ends flag parsing,
// so every word after it stays positional. A repeated flag keeps its last
// value. It returns the positional words, an exit code and whether parsing
// succeeded; -h prints the usage and is code 0 with ok false.
func parseFlagsAnywhere(flags *flag.FlagSet, args []string) ([]string, int, bool) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0, false
			}
			return nil, 2, false
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return positional, 0, true
		}
		if len(rest) < len(args) && args[len(args)-len(rest)-1] == "--" {
			return append(positional, rest...), 0, true
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// QuietAfterEnv names the variable that replaces callmeter.QuietAfter for one
// report run: a Go duration, zero or more. The e2e sets it low to settle a
// session it has just run; unset, recovery waits QuietAfter.
const QuietAfterEnv = "CALLMETER_QUIET_AFTER"

// quietAfter is the quiet recovery waits for this run, from QuietAfterEnv;
// false, after a line on stderr, when the value is not a duration of zero or
// more.
func quietAfter(getenv paths.Getenv, stderr io.Writer) (time.Duration, bool) {
	value := getenv(QuietAfterEnv)
	if value == "" {
		return callmeter.QuietAfter, true
	}
	quiet, err := time.ParseDuration(value)
	if err != nil || quiet < 0 {
		fmt.Fprintf(stderr, "callmeter report: %s=%q is not a duration of zero or more, such as 1ms or 2h\n", QuietAfterEnv, value)
		return 0, false
	}
	return quiet, true
}

func reportAction(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	getenv paths.Getenv,
) (exitCode int) {
	flags, values := newFlags("callmeter report", stderr)
	positional, code, ok := parseFlagsAnywhere(flags, args)
	if !ok {
		if code != 0 {
			pluginLog(stderr, getenv, "info", "usage-refused", "internal/callmeter/command/command.go#reportAction", "invalid report flags or filters")
		}
		return code
	}
	var topic topicFunc
	if len(positional) == 1 {
		topic = topics[positional[0]]
	}
	if topic == nil {
		fmt.Fprintf(stderr, "callmeter report: want one topic, got %q\n", positional)
		flags.Usage()
		pluginLog(stderr, getenv, "info", "usage-refused", "internal/callmeter/command/command.go#reportAction", "report requires one known topic")
		return 2
	}
	now := clock.Real.Now()
	filter, code, ok := buildFilter(values, now, stderr)
	if !ok {
		if code != 0 {
			pluginLog(stderr, getenv, "info", "usage-refused", "internal/callmeter/command/command.go#reportAction", "invalid report flags or filters")
		}
		return code
	}
	quiet, ok := quietAfter(getenv, stderr)
	if !ok {
		pluginLog(stderr, getenv, "info", "usage-refused", "internal/callmeter/command/command.go#reportAction", "invalid quiet-after duration")
		return 2
	}
	home, err := paths.Home(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: %v\n", err)
		pluginLog(stderr, getenv, "error", "store-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	cfg, envErr, err := config.Resolve(paths.Config(home), getenv)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: %v\n", err)
		pluginLog(stderr, getenv, "error", "config-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	if envErr != nil {
		// A bad variable never stops the report: the default (keep every row) stands.
		fmt.Fprintf(stderr, "callmeter: %v; using the default, keeping every row\n", envErr)
		pluginLog(stderr, getenv, "error", "config-env-invalid", "internal/callmeter/command/command.go#reportAction", envErr.Error())
	}
	before := filter.Since
	filter.Since = windowSince(filter.Since, values.since, cfg.RetentionDays, now, stderr)
	if !before.IsZero() && !before.Equal(filter.Since) {
		pluginLog(stderr, getenv, "info", "retention-clamped", "internal/callmeter/command/command.go#windowSince", "report window clamped to configured retention")
	}
	path := paths.Store(home)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		// No store to ingest into: the lines still waiting in missed.log and its
		// claims are lost events, counted read-only and named as a failure.
		counts, err := callmeter.CountMissed(paths.Missed(paths.LogDir(getenv)))
		if err != nil {
			fmt.Fprintf(stderr, "callmeter: count missed.log: %v\n", err)
			pluginLog(stderr, getenv, "error", "missed-count-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
			return 1
		}
		if err := printNoStore(stdout, values.json, positional[0], path, missedNotes(counts)); err != nil {
			fmt.Fprintf(stderr, "callmeter: report %s: %v\n", positional[0], err)
			pluginLog(stderr, getenv, "error", "report-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
			return 1
		}
		return 0
	}
	db, err := callmeter.OpenDB(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: cannot open store %s: %v\n", path, err)
		pluginLog(stderr, getenv, "error", "store-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(stderr, "callmeter: close store: %v\n", err)
			pluginLog(stderr, getenv, "error", "store-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}()
	db.SetRetention(cfg.Retention())
	names := &transcriptNames{ctx: ctx, db: db.DB(), getenv: getenv, stderr: stderr}
	if _, err := db.IngestMissed(ctx, paths.Missed(paths.LogDir(getenv))); err != nil {
		fmt.Fprintf(stderr, "callmeter: ingest missed.log: %v\n", err)
		pluginLog(stderr, getenv, "error", "missed-ingest-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	if _, err := report.PruneExpired(ctx, db, now); err != nil {
		fmt.Fprintf(stderr, "callmeter: %v\n", err)
		pluginLog(stderr, getenv, "warning", "prune-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	// Sessions gone quiet are settled from their transcripts before the topic
	// reads the store: a call a killed session left in flight, a request no
	// hook swept.
	recovered, err := db.RecoverQuiet(ctx, now, quiet)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: recover quiet sessions: %v\n", err)
		pluginLog(stderr, getenv, "warning", "recover-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	recovery := recoveryNotes(recovered.Skipped)
	for _, skipped := range recovered.Skipped {
		fmt.Fprintf(stderr, "callmeter: recover quiet sessions: skipped: %v\n", skipped)
		row := pluginlog.Row{Level: "warning", Event: "recover-skipped", Where: "internal/callmeter/command/command.go#reportAction", Detail: skippedDetail(skipped)}
		var read *callmeter.SkippedRead
		if errors.As(skipped, &read) {
			row.Session = read.Session
		}
		pluginLogRow(stderr, getenv, row)
	}
	if _, err := report.EnsureParsed(ctx, db, getenv("HOME"), nil); err != nil {
		fmt.Fprintf(stderr, "callmeter: parse commands: %v\n", err)
		pluginLog(stderr, getenv, "error", "parse-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	// Calls no transcript records come in from the optional cost ledger, and every
	// settled request gets its price, before the topic reads the store.
	if _, _, err := db.PriceUnpriced(ctx); err != nil {
		fmt.Fprintf(stderr, "callmeter: price requests: %v\n", err)
		pluginLog(stderr, getenv, "error", "price-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	filter.Ledger = ingestLedger(ctx, db, getenv, now, stderr)
	// An own seat that cannot be resolved stays empty: coverage names it in a note.
	filter.OwnSeat, _ = paths.SeatDir(getenv)
	table, err := topic(ctx, db, filter, names.nameOf)
	if err == nil {
		table.Notes = append(table.Notes, report.InapplicableNotes(positional[0], filter)...)
		table.Notes = append(table.Notes, recovery...)
		var older []string
		older, err = report.OlderRulesNotes(ctx, db)
		table.Notes = append(table.Notes, older...)
	}
	if err == nil {
		if values.json {
			err = table.RenderJSON(stdout, positional[0])
		} else {
			err = table.Render(stdout)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: report %s: %v\n", positional[0], err)
		pluginLog(stderr, getenv, "error", "report-failed", "internal/callmeter/command/command.go#reportAction", err.Error())
		return 1
	}
	return 0
}

// ingestLedger brings the cost ledger in. A ledger that cannot be read is one
// stderr line and a summary naming the failure, never "no ledger": the reports
// still run, and the cost topic says its side calls are not counted.
func ingestLedger(ctx context.Context, db *callmeter.Store, getenv paths.Getenv, now time.Time, stderr io.Writer) *callmeter.LedgerSummary {
	dir := paths.CostLedger(getenv)
	if dir == "" {
		return &callmeter.LedgerSummary{} // no ledger is set
	}
	summary, err := db.IngestLedger(ctx, dir, now)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: cost ledger: %v\n", err)
		pluginLog(stderr, getenv, "warning", "ledger-unreadable", "internal/callmeter/command/command.go#ingestLedger", err.Error())
		summary.Err = err.Error()
	}
	return &summary
}

// recoveryNotes summarizes skipped transcripts without exposing paths or OS
// errors, counting a transcript skipped by more than one read once.
func recoveryNotes(skipped []error) []string {
	if len(skipped) == 0 {
		return nil
	}
	transcripts := 0
	seen := map[callmeter.SkippedRead]bool{}
	for _, err := range skipped {
		var read *callmeter.SkippedRead
		if errors.As(err, &read) {
			key := callmeter.SkippedRead{Session: read.Session, Path: read.Path}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		transcripts++
	}
	label := "unreadable"
	switch {
	case errors.Is(skipped[0], fs.ErrPermission):
		label = "permission denied"
	case errors.Is(skipped[0], fs.ErrNotExist):
		label = "not found"
	}
	return []string{fmt.Sprintf("%d transcripts could not be read by recovery (first: %s)", transcripts, label)}
}

// unrecordedWhy names each stage of a missed.log line as the store's own
// "events unrecorded" notes do (report.recordingNotes), in their order.
var unrecordedWhy = []struct{ stage, why string }{
	{callmeter.StageBinary, "binary unavailable"},
	{callmeter.StageTerminated, "hook terminated before recording"},
}

// missedNotes is the one failure note over missed lines counted with no store,
// `{n} events unrecorded: {why}: {n}, …`; none when no line waits.
func missedNotes(counts map[string]int) []string {
	total, parts := 0, []string{}
	for _, lost := range unrecordedWhy {
		if n := counts[lost.stage]; n > 0 {
			total += n
			parts = append(parts, fmt.Sprintf("%s: %d", lost.why, n))
		}
	}
	if total == 0 {
		return []string{}
	}
	return []string{fmt.Sprintf("%d events unrecorded: %s", total, strings.Join(parts, ", "))}
}

// printNoStore says no store file exists: a line on stdout, or with --json the
// object whose `store` is "absent". With notes (events lost before any store
// existed) the line names only the absence and each note follows it as a
// `note:` line, never "nothing recorded yet".
func printNoStore(stdout io.Writer, asJSON bool, topic, path string, notes []string) error {
	if !asJSON {
		if len(notes) == 0 {
			_, err := fmt.Fprintf(stdout, "callmeter: no store at %s: nothing recorded yet\n", path)
			return err
		}
		if _, err := fmt.Fprintf(stdout, "callmeter: no store at %s\n", path); err != nil {
			return err
		}
		for _, note := range notes {
			if _, err := fmt.Fprintln(stdout, "note: "+note); err != nil {
				return err
			}
		}
		return nil
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(struct {
		Topic   string     `json:"topic"`
		Store   string     `json:"store"`
		Path    string     `json:"path"`
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
		Notes   []string   `json:"notes"`
	}{topic, "absent", path, []string{}, [][]string{}, notes})
}

// buildFilter turns the flags into a report.Filter: --since a time or, unset,
// none (the whole store; windowSince bounds it by a retention), --project
// absolute.
func buildFilter(values *flagValues, now time.Time, stderr io.Writer) (report.Filter, int, bool) {
	since, err := report.ParseSince(values.since, now)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return report.Filter{}, 2, false
	}
	project := values.project
	if project != "" {
		if project, err = filepath.Abs(project); err != nil {
			fmt.Fprintf(stderr, "callmeter: --project %s: %v\n", values.project, err)
			return report.Filter{}, 2, false
		}
	}
	if values.view != "" && !slices.Contains(report.CostViews, values.view) {
		fmt.Fprintf(stderr, "callmeter: --view %q is none of %s\n", values.view, strings.Join(report.CostViews, ", "))
		return report.Filter{}, 2, false
	}
	return report.Filter{
		Since: since, Project: project, AgentType: values.agentType, Session: values.session,
		Limit: values.limit, CostView: values.view,
	}, 0, true
}

// windowSince bounds a report's since by the retention window in days, from
// CALLMETER_RETENTION_DAYS or else config.json (config.Resolve); 0 keeps every row, so since stands. With a window, an unset
// since is its start and one older than it is clamped to it with one stderr
// note, since the store holds nothing before it. raw is the --since text.
func windowSince(since time.Time, raw string, retentionDays int, now time.Time, stderr io.Writer) time.Time {
	if retentionDays == 0 {
		return since
	}
	floor := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	switch {
	case since.IsZero():
		return floor
	case since.Before(floor):
		fmt.Fprintf(stderr, "callmeter: --since %s is older than the %d-day retention window; clamped to %s\n",
			raw, retentionDays, floor.UTC().Format(time.RFC3339))
		return floor
	}
	return since
}

// pluginLog adds metadata beside existing CLI output, never changing it.
func pluginLog(stderr io.Writer, getenv paths.Getenv, level, event, where, msg string) {
	pluginLogRow(stderr, getenv, pluginlog.Row{Level: level, Event: event, Msg: msg, Where: where})
}

// logLabels is a detail of fixed-vocabulary labels, safe to log at error and
// warning level: no producer text, no path.
type logLabels map[string]string

func pluginLogRow(stderr io.Writer, getenv paths.Getenv, row pluginlog.Row) {
	// Producer errors may quote rules, config or transcript contents. Keep the
	// operation category and provenance here; the existing CLI diagnostic is unchanged.
	// Only logLabels, fixed-vocabulary labels a producer built, pass as detail.
	if row.Level == "error" || row.Level == "warning" {
		row.Msg = strings.ReplaceAll(row.Event, "-", " ")
		if _, labels := row.Detail.(logLabels); !labels {
			row.Detail = nil
		}
	}
	faultlog.Level(stderr, faultlog.ResolveSink(getenv), row)
}

// skippedDetail says why recovery skipped a transcript read, in labels only:
// the stage of the read (SkippedRead.Stage) and a reason from a fixed
// vocabulary. The path and the error text stay out, since the error may quote
// transcript content (the plugin log strips them for the same reason).
func skippedDetail(err error) logLabels {
	detail := logLabels{"reason": "unreadable"}
	var read *callmeter.SkippedRead
	if errors.As(err, &read) && read.Stage != "" {
		detail["stage"] = read.Stage
	}
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case read != nil && read.Stage == callmeter.StageSanitizeInput:
		detail["reason"] = "refused" // read in full; its input was not taken
	case errors.Is(err, fs.ErrNotExist):
		detail["reason"] = "not found"
	case errors.Is(err, fs.ErrPermission):
		detail["reason"] = "permission denied"
	case errors.As(err, &syntax), errors.As(err, &typed):
		detail["reason"] = "malformed"
	}
	return detail
}
