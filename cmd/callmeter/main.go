// Command callmeter records Claude Code hook events into a local call store
// and reports on it: `callmeter hook` is the hook entry, `callmeter report`
// the reports over what it recorded.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/rezzminator/callmeter/internal/applog"
	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/command"
	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/hookentry"
	"github.com/rezzminator/callmeter/internal/paths"
)

// version is set at build time: -ldflags "-X main.version={v}".
var version = "dev"

const usage = `usage: callmeter {hook|report|redact|version|help}
  hook      record the hook payload on stdin into the call store
  report    print a report over the call store
  redact    rewrite the rows already stored under the privacy rules (never automatic)
  version   print the version
  help      print this text

` + command.Usage

// hook is the hook entry; a variable so a test can make it panic.
var hook = hookentry.Callmeter

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run is the binary without the process around it: args are the arguments
// after the program name. It returns the exit code: `hook` always 0, `report`
// 0, 1 or 2, a usage error 2.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv paths.Getenv) int {
	if len(args) > 0 {
		switch args[0] {
		case "hook":
			return runHook(stdin, stderr, getenv)
		case "report":
			return command.CLI(args, stdout, stderr, getenv)
		case "redact":
			return command.Redact(args, stdout, stderr, getenv)
		case "version":
			fmt.Fprintf(stdout, "callmeter %s\n", version)
			return 0
		case "help", "-h", "--help":
			fmt.Fprintln(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "callmeter: unknown command %q\n", args[0])
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

// runHook is the hook entry with a net under it: a hook exits 0 on every path,
// and a panic would exit 2, which Claude Code reads as a blocking error. A
// recovered panic still prints its stack; an event not yet accounted for also
// leaves one missed.log line with callmeter.PanicReason and one log line, so
// the next run counts the lost event as a `terminated` fault.
func runHook(stdin io.Reader, stderr io.Writer, getenv paths.Getenv) (code int) {
	read := &payloadPrefix{}
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			accounted, ok := recovered.(hookentry.AccountedPanic)
			if ok {
				recovered = accounted.Value
			}
			fmt.Fprintf(stderr, "callmeter: hook panicked: %v\n%s", recovered, stack)
			if !ok {
				leavePanicLine(read.data, recovered, stack, stderr, getenv)
			}
			code = 0
		}
	}()
	return hook(io.TeeReader(stdin, read), stderr, getenv)
}

// leavePanicLine appends the missed.log line and the log line of a recovered
// hook panic; with no home resolvable, stderr is all there is.
func leavePanicLine(prefix []byte, recovered any, stack []byte, stderr io.Writer, getenv paths.Getenv) {
	home, err := paths.Home(getenv)
	if err != nil {
		applog.Failure(stderr, "", callmeter.StageTerminated, "", "", fmt.Errorf("resolve callmeter home: %w", err))
		return
	}
	event, session := panicIDs(prefix)
	logPath := paths.Log(home)
	if err := hookentry.AppendMissed(
		paths.Missed(home), event, session, callmeter.PanicReason, clock.Real.Now()); err != nil {
		applog.Failure(stderr, logPath, callmeter.StageTerminated, session, "", err)
	}
	applog.Failure(stderr, logPath, callmeter.StageTerminated, session, "",
		fmt.Errorf("hook panicked: %v\n%s", recovered, stack))
}

// payloadPrefixLimit bounds what runHook keeps of the payload for panicIDs:
// Claude Code writes session_id and hook_event_name among the first fields,
// before any tool input or response.
const payloadPrefixLimit = 64 << 10

// payloadPrefix keeps the first payloadPrefixLimit bytes written to it and
// drops the rest, never failing the write, so the tee under the hook never
// changes what the hook reads.
type payloadPrefix struct{ data []byte }

func (p *payloadPrefix) Write(b []byte) (int, error) {
	if room := payloadPrefixLimit - len(p.data); room > 0 {
		p.data = append(p.data, b[:min(room, len(b))]...)
	}
	return len(b), nil
}

// panicIDs reads hook_event_name and session_id from the top-level object of a
// payload prefix, as far as it decodes: a truncated or broken payload yields
// what came before the break, and an empty name for what never came.
func panicIDs(prefix []byte) (event, session string) {
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return "", ""
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return event, session
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return event, session
		}
		var text string
		switch key {
		case "hook_event_name":
			if json.Unmarshal(value, &text) == nil {
				event = text
			}
		case "session_id":
			if json.Unmarshal(value, &text) == nil {
				session = text
			}
		}
	}
	return event, session
}
