// Command callmeter records Claude Code hook events into a local call store
// and reports on it: `callmeter hook` is the hook entry, `callmeter report`
// the reports over what it recorded.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/rezzminator/callmeter/internal/callmeter/command"
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
// and a panic would exit 2, which Claude Code reads as a blocking error.
func runHook(stdin io.Reader, stderr io.Writer, getenv paths.Getenv) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(stderr, "callmeter: hook panicked: %v\n%s", recovered, debug.Stack())
			code = 0
		}
	}()
	return hook(stdin, stderr, getenv)
}
