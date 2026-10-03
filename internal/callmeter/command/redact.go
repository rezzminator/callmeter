package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/paths"
)

// RedactUsage is the redact action's usage text.
const RedactUsage = `usage: callmeter redact
  rewrite the rows already stored under the privacy rules (heredoc bodies cut
  out of Bash commands, free text and tool output replaced by its size), in one
  transaction, and print the rows changed per column; never run automatically`

// Redact is `callmeter redact`: callmeter.Store.Redact over the store under
// CALLMETER_HOME, printing the rows changed per column. args starts with
// "redact". It returns the process exit code: 0 done (or no store to
// rewrite), 1 a store error, 2 usage.
func Redact(args []string, stdout, stderr io.Writer, getenv paths.Getenv) int {
	if len(args) != 1 || args[0] != "redact" {
		fmt.Fprintln(stderr, RedactUsage)
		return 2
	}
	home, err := paths.Home(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter redact: %v\n", err)
		return 1
	}
	path := paths.Store(home)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "callmeter redact: no store at %s, nothing to rewrite\n", path)
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "callmeter redact: store %s: %v\n", path, err)
		return 1
	}
	ctx := context.Background()
	db, err := callmeter.OpenDB(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "callmeter redact: %v\n", err)
		return 1
	}
	counts, err := db.Redact(ctx)
	if closeErr := db.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close store %s: %w", path, closeErr))
	}
	if err != nil {
		fmt.Fprintf(stderr, "callmeter redact: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "callmeter redact: %s\n", path)
	for _, c := range counts {
		what := "rows rewritten"
		if c.Column == "command_parts" {
			what = "rows deleted (parsed again by the next report)"
		}
		fmt.Fprintf(stdout, "%-15s %d %s\n", c.Column, c.Rows, what)
	}
	return 0
}
