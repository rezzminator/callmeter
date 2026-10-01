// Package runner is the one seam every child process crosses: callers hold a
// Runner, Real drives os/exec, and a test hands in a Runner of its own.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// RunOptions configures one Runner.Run call: the environment and working
// directory the child process inherits, and the input fed on its stdin. A nil
// Env inherits the caller's own environment and an empty Dir runs in the
// caller's own working directory — the same defaults a bare exec.Cmd takes
// when those fields are left unset.
type RunOptions struct {
	Env   []string
	Dir   string
	Stdin []byte
}

// RunResult is one command's completed run: stdout and stderr split, plus the
// process exit code, or -1 when the process never reached one (the binary
// could not start, the context was cancelled first).
type RunResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner runs a command to completion and resolves a program on PATH.
type Runner interface {
	Run(ctx context.Context, argv []string, opts RunOptions) (RunResult, error)
	LookPath(name string) (string, error)
}

// Real runs argv through os/exec.
type Real struct{}

// Run starts argv[0] with argv[1:] as its arguments. A nonzero exit is the
// command answering, not the Runner failing to run it — Run returns a nil
// error with ExitCode set; only a failure to start (lookup, permission,
// context already done) returns a non-nil error, with ExitCode -1.
func (Real) Run(ctx context.Context, argv []string, opts RunOptions) (RunResult, error) {
	if len(argv) == 0 {
		return RunResult{ExitCode: -1}, errors.New("runner: Real.Run: empty argv")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = opts.Env
	command.Dir = opts.Dir
	if opts.Stdin != nil {
		command.Stdin = bytes.NewReader(opts.Stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	// A nil err IS the zero exit code; -1 is only for a process that never
	// reached one.
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		exitCode = -1
	}
	result := RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode}
	if err != nil && exitErr == nil {
		return result, fmt.Errorf("run %q: %w", argv[0], err)
	}
	return result, nil
}

// LookPath resolves name on $PATH exactly as exec.LookPath does.
func (Real) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}
