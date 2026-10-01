package runner

import (
	"context"
	"testing"
)

var _ Runner = Real{}

func TestRealRunCapturesStdoutStderrAndExitCode(t *testing.T) {
	var runner Real
	result, err := runner.Run(
		context.Background(),
		[]string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"},
		RunOptions{},
	)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (a nonzero exit is not a Runner failure)", err)
	}
	if got := string(result.Stdout); got != "out\n" {
		t.Fatalf("Stdout = %q, want %q", got, "out\n")
	}
	if got := string(result.Stderr); got != "err\n" {
		t.Fatalf("Stderr = %q, want %q", got, "err\n")
	}
	if result.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", result.ExitCode)
	}
}

func TestRealRunReturnsAnErrorWhenTheBinaryCannotStart(t *testing.T) {
	var runner Real
	result, err := runner.Run(context.Background(), []string{"callmeter-runner-test-no-such-binary"}, RunOptions{})
	if err == nil {
		t.Fatal("Run() with a nonexistent binary returned nil error")
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 for a process that never ran", result.ExitCode)
	}
}

func TestRealRunRefusesAnEmptyArgv(t *testing.T) {
	var runner Real
	result, err := runner.Run(context.Background(), nil, RunOptions{})
	if err == nil {
		t.Fatal("Run() with an empty argv returned nil error")
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", result.ExitCode)
	}
}

func TestRealRunRespectsEnvDirAndStdin(t *testing.T) {
	var runner Real
	dir := t.TempDir()
	result, err := runner.Run(context.Background(), []string{"/bin/sh", "-c", "echo \"$GREETING $(cat) $(pwd -P)\""}, RunOptions{
		Env:   []string{"GREETING=hi"},
		Dir:   dir,
		Stdin: []byte("there"),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := "hi there " + dir + "\n"
	if got := string(result.Stdout); got != want {
		t.Fatalf("Stdout = %q, want %q", got, want)
	}
}

func TestRealLookPathResolvesAKnownBinary(t *testing.T) {
	var runner Real
	path, err := runner.LookPath("sh")
	if err != nil {
		t.Fatalf("LookPath(sh) error = %v", err)
	}
	if path == "" {
		t.Fatal("LookPath(sh) returned an empty path with a nil error")
	}
	if _, err := runner.LookPath("callmeter-runner-test-no-such-binary"); err == nil {
		t.Fatal("LookPath of a missing program returned nil error")
	}
}
