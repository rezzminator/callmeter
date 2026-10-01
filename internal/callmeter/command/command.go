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
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/report"
	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
)

// Usage is the report action's usage text.
const Usage = `usage: callmeter report {files|writes|commands|context|sequences|faults|sessions|prompts|effort|tokens|agents|outcomes|coverage|events} [--since D] [--project P]
                       [--agent-type T] [--session S] [--limit N] [--json]
  --since D         a duration (7d, 24h) or a date (2026-09-01); default and floor: the 30-day retention window
  --json            one JSON object on stdout instead of the text table`

type topicFunc func(context.Context, *callmeter.Store, report.Filter, report.NameOf) (*report.Table, error)

var topics = map[string]topicFunc{
	"files": report.Files, "writes": report.Writes, "commands": report.Commands,
	"context": report.Context, "sequences": report.Sequences, "faults": report.Faults,
	"sessions": report.Sessions, "prompts": report.Prompts, "effort": report.Effort, "tokens": report.Tokens,
	"agents": report.Agents, "outcomes": report.Outcomes, "coverage": report.Coverage, "events": report.Events,
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
	return 2
}

type flagValues struct {
	since, project, agentType, session string
	limit                              int
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

func reportAction(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	getenv paths.Getenv,
) (exitCode int) {
	flags, values := newFlags("callmeter report", stderr)
	positional, code, ok := parseFlagsAnywhere(flags, args)
	if !ok {
		return code
	}
	var topic topicFunc
	if len(positional) == 1 {
		topic = topics[positional[0]]
	}
	if topic == nil {
		fmt.Fprintf(stderr, "callmeter report: want one topic, got %q\n", positional)
		flags.Usage()
		return 2
	}
	now := clock.Real.Now()
	filter, code, ok := buildFilter(values, now, stderr)
	if !ok {
		return code
	}
	home, err := paths.Home(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: %v\n", err)
		return 1
	}
	path := paths.Store(home)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := printNoStore(stdout, values.json, positional[0], path); err != nil {
			fmt.Fprintf(stderr, "callmeter: report %s: %v\n", positional[0], err)
			return 1
		}
		return 0
	}
	db, err := callmeter.OpenDB(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: cannot open store %s: %v\n", path, err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(stderr, "callmeter: close store: %v\n", err)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}()
	names := &transcriptNames{ctx: ctx, db: db.DB(), getenv: getenv}
	if _, err := db.IngestMissed(ctx, paths.Missed(home)); err != nil {
		fmt.Fprintf(stderr, "callmeter: ingest missed.log: %v\n", err)
		return 1
	}
	if _, err := report.PruneExpired(ctx, db, now); err != nil {
		fmt.Fprintf(stderr, "callmeter: %v\n", err)
		return 1
	}
	if _, err := report.EnsureParsed(ctx, db, getenv("HOME"), nil); err != nil {
		fmt.Fprintf(stderr, "callmeter: parse commands: %v\n", err)
		return 1
	}
	// An own seat that cannot be resolved stays empty: coverage names it in a note.
	filter.OwnSeat, _ = paths.SeatDir(getenv)
	table, err := topic(ctx, db, filter, names.nameOf)
	if err == nil {
		table.Notes = append(table.Notes, report.InapplicableNotes(positional[0], filter)...)
		if values.json {
			err = table.RenderJSON(stdout, positional[0])
		} else {
			err = table.Render(stdout)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "callmeter: report %s: %v\n", positional[0], err)
		return 1
	}
	return 0
}

// printNoStore says no store file exists: a line on stdout, or with --json the
// object whose `store` is "absent".
func printNoStore(stdout io.Writer, asJSON bool, topic, path string) error {
	if !asJSON {
		_, err := fmt.Fprintf(stdout, "callmeter: no store at %s: nothing recorded yet\n", path)
		return err
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
	}{topic, "absent", path, []string{}, [][]string{}, []string{}})
}

// buildFilter turns the flags into a report.Filter: --since clamped to the
// retention window (with one note), --project absolute.
func buildFilter(values *flagValues, now time.Time, stderr io.Writer) (report.Filter, int, bool) {
	since, err := report.ParseSince(values.since, now)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return report.Filter{}, 2, false
	}
	if floor := now.Add(-report.Retention); since.Before(floor) {
		fmt.Fprintf(stderr, "callmeter: --since %s is older than the %d-day retention window; clamped to %s\n",
			values.since, int(report.Retention.Hours()/24), floor.UTC().Format(time.RFC3339))
		since = floor
	}
	project := values.project
	if project != "" {
		if project, err = filepath.Abs(project); err != nil {
			fmt.Fprintf(stderr, "callmeter: --project %s: %v\n", values.project, err)
			return report.Filter{}, 2, false
		}
	}
	return report.Filter{
		Since: since, Project: project, AgentType: values.agentType, Session: values.session,
		Limit: values.limit,
	}, 0, true
}
