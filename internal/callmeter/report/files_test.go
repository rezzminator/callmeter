package report

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func read(id, agent, path string, age time.Duration, delivered int64) callmeter.Call {
	return callmeter.Call{
		ToolUseID: id, SessionID: callmeter.Ptr("s"), AgentID: callmeter.Ptr(agent), TS: callmeter.Ptr(ms(age)),
		Tool: callmeter.Ptr("Read"), FilePath: callmeter.Ptr(path), BytesDelivered: callmeter.Ptr(delivered),
	}
}

func TestFilesKeepsReadAndBashBytesApartAndCountsRereads(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "f.go")
	path := filepath.Join(dir, "f.go")
	first := read("r1", "A", path, 4*time.Hour, 100)
	first.FileBytes = callmeter.Ptr(int64(500))
	seed(t, store, first)
	ranged := read("r2", "A", path, 3*time.Hour, 50)
	ranged.ReadStart = callmeter.Ptr(int64(10))
	seed(t, store, ranged)
	last := read("r3", "B", path, 2*time.Hour, 30)
	last.FileBytes = callmeter.Ptr(int64(999))
	seed(t, store, last)
	seed(t, store, bash("b1", "s", "A", ms(time.Hour), dir, "cat f.go", 70))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := [][]string{{path, "180", "70", "4", "4", "0", "2", "999", "3", "1", "2", "0"}}
	if !reflect.DeepEqual(table.Rows, want) {
		t.Errorf("rows = %v\nwant   %v (header %v)", table.Rows, want, table.Header)
	}
}

func TestFilesSplitsCertainAndConditionalReads(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "f.go")
	path := filepath.Join(dir, "f.go")
	seed(t, store, bash("b1", "s", "A", ms(3*time.Hour), dir, "cat f.go", 10))
	seed(t, store, bash("b2", "s", "A", ms(2*time.Hour), dir, "true && cat f.go", 20))
	seed(t, store, bash("b3", "s", "A", ms(time.Hour), dir, "true || wc -l f.go; cat f.go", 30))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	// b3 reads f.go both ways in one call: a certain read wins.
	want := [][]string{{path, "0", "60", "3", "2", "1", "1", "-", "3", "0", "2", "0"}}
	if !reflect.DeepEqual(table.Rows, want) {
		t.Errorf("rows = %v\nwant   %v (header %v)", table.Rows, want, table.Header)
	}
}

func TestWritesSumsGrowthAndCountsBashWrites(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "out.txt")
	write := func(id, tool string, before, after *int64) callmeter.Call {
		return callmeter.Call{
			ToolUseID: id, SessionID: callmeter.Ptr("s"), TS: callmeter.Ptr(ms(time.Hour)), Tool: callmeter.Ptr(tool),
			FilePath: callmeter.Ptr("/w/f.go"), FileBytesBefore: before, FileBytes: after,
		}
	}
	seed(t, store, write("w1", "Write", callmeter.Ptr(int64(100)), callmeter.Ptr(int64(150))))
	seed(t, store, write("w2", "Edit", callmeter.Ptr(int64(150)), callmeter.Ptr(int64(140))))
	seed(t, store, write("w3", "Edit", nil, callmeter.Ptr(int64(160))))
	seed(t, store, bash("b1", "s", "", ms(time.Hour), dir, "echo hi > out.txt", 0))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Writes(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Writes: %v", err)
	}
	want := [][]string{
		{"/w/f.go", "3", "3", "0", "40", "1"},
		{filepath.Join(dir, "out.txt"), "1", "0", "1", "0", "1"},
	}
	if !reflect.DeepEqual(table.Rows, want) {
		t.Errorf("rows = %v\nwant   %v (header %v)", table.Rows, want, table.Header)
	}
}

// bashBytesOf returns the BASH BYTES column per file.
func bashBytesOf(t *testing.T, table *Table) map[string]string {
	t.Helper()
	col := -1
	for i, h := range table.Header {
		if h == "BASH BYTES" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("no BASH BYTES column in %v", table.Header)
	}
	per := map[string]string{}
	for _, row := range table.Rows {
		per[row[0]] = row[col]
	}
	return per
}

// A Bash call's bytes are credited to a file only when the call reads that
// one file and runs no file: the output of a call naming several files, or
// also running one, cannot be told apart per file, so its reads count and
// its bytes are credited to none, with a note.
func TestFilesCreditsBashBytesOnlyToASingleFileRead(t *testing.T) {
	cases := []struct {
		name     string
		files    []string
		commands []string
		want     map[string]string // file name -> BASH BYTES
		shared   int64             // calls whose bytes no file gets
	}{
		{
			"three files in one part",
			[]string{"a.go", "b.go", "c.go"},
			[]string{"cat a.go b.go c.go"},
			map[string]string{"a.go": UnknownSize, "b.go": UnknownSize, "c.go": UnknownSize}, 1,
		},
		{
			"one file named twice is a single-file read",
			[]string{"a.go"},
			[]string{"cat a.go; head -n 2 a.go"},
			map[string]string{"a.go": "100"}, 0,
		},
		{
			"two parts naming two files",
			[]string{"a.go", "b.go"},
			[]string{"cat a.go; head -n 2 a.go b.go"},
			map[string]string{"a.go": UnknownSize, "b.go": UnknownSize}, 1,
		},
		{
			"grep -l over a glob",
			[]string{"p.go", "q.go"},
			[]string{"grep -l x *.go"},
			map[string]string{"p.go": UnknownSize, "q.go": UnknownSize}, 1,
		},
		{
			"a single-file read beside a multi-file one keeps its own bytes",
			[]string{"a.go", "b.go"},
			[]string{"cat a.go", "cat a.go b.go"},
			map[string]string{"a.go": "100", "b.go": UnknownSize}, 1,
		},
		{
			"a call that also runs the file it reads",
			[]string{"run.sh"},
			[]string{"./run.sh; sed -n 1,3p run.sh"},
			map[string]string{"run.sh": UnknownSize}, 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			dir := workDir(t, c.files...)
			for i, command := range c.commands {
				seed(t, store, bash("b"+strconv.Itoa(i), "s", "A", ms(time.Duration(len(c.commands)-i)*time.Hour), dir, command, 100))
			}
			if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
				t.Fatalf("EnsureParsed: %v", err)
			}
			table, err := Files(ctx, store, Filter{}, nil)
			if err != nil {
				t.Fatalf("Files: %v", err)
			}
			want := map[string]string{}
			for name, v := range c.want {
				want[filepath.Join(dir, name)] = v
			}
			if per := bashBytesOf(t, table); !reflect.DeepEqual(per, want) {
				t.Errorf("BASH BYTES per file = %v\nwant %v", per, want)
			}
			out := render(t, table)
			note := sharedBytesNote(c.shared)
			if c.shared > 0 && !strings.Contains(out, "note: "+note) {
				t.Errorf("report lacks %q:\n%s", note, out)
			}
			if c.shared == 0 && strings.Contains(out, "credited to no file") {
				t.Errorf("report names shared bytes with none shared:\n%s", out)
			}
		})
	}
}

// Only a read mode counts as a read: running a file is an exec, counted in
// EXECS alone, and a wrapper or unrecognised program naming it (mode
// unknown) counts nowhere and is named in a note. Neither credits its bytes.
func TestFilesCountsOnlyReadModes(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "run.sh")
	path := filepath.Join(dir, "run.sh")
	seed(t, store, bash("b1", "s", "A", ms(3*time.Hour), dir, "./run.sh", 500))
	seed(t, store, bash("b2", "s", "A", ms(2*time.Hour), dir, "wrapit run.sh", 700))
	seed(t, store, bash("b3", "s", "A", ms(time.Hour), dir, "sed -n 1,5p run.sh", 40))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := [][]string{{path, "0", "40", "1", "1", "0", "1", "-", "0", "1", "0", "1"}}
	if !reflect.DeepEqual(table.Rows, want) {
		t.Errorf("rows = %v\nwant   %v (header %v)", table.Rows, want, table.Header)
	}
	out := render(t, table)
	note := "note: 1 file attributions with mode unknown (a wrapper or unrecognised program): not counted"
	if !strings.Contains(out, note) {
		t.Errorf("report lacks %q:\n%s", note, out)
	}
	if strings.Contains(out, "credited to no file") {
		t.Errorf("report names shared bytes with none shared:\n%s", out)
	}
}

// A file the parse found missing (a scratch file since deleted) is a read
// like any other; its SIZE says gone, never a 0
// or the "-" of a size never seen.
func TestFilesMarksAMissingFileGone(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "a.go")
	seed(t, store, bash("b1", "s", "A", ms(time.Hour), dir, "cat a.go gone.go", 100))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := [][]string{
		{filepath.Join(dir, "a.go"), "0", UnknownSize, "1", "1", "0", "1", "-", "1", "0", "0", "0"},
		{filepath.Join(dir, "gone.go"), "0", UnknownSize, "1", "1", "0", "1", "gone", "1", "0", "0", "0"},
	}
	if !reflect.DeepEqual(table.Rows, want) {
		t.Errorf("rows = %v\nwant   %v (header %v)", table.Rows, want, table.Header)
	}
}

// A Read or Bash call with no delivered size is left out of READ BYTES and
// BASH BYTES, never added as 0 bytes: a file whose only sizes are unknown
// shows them as unknown, and a note names the calls.
func TestFilesLeavesUnknownSizesOutOfBytes(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "x.go", "y.go")
	x, y := filepath.Join(dir, "x.go"), filepath.Join(dir, "y.go")
	onlyUnknown := read("r1", "A", x, 4*time.Hour, 0)
	onlyUnknown.BytesDelivered = nil
	seed(t, store, onlyUnknown)
	seed(t, store, read("r2", "A", y, 3*time.Hour, 100))
	partly := read("r3", "B", y, 2*time.Hour, 0)
	partly.BytesDelivered = nil
	seed(t, store, partly)
	cat := bash("b1", "s", "A", ms(time.Hour), dir, "cat x.go", 0)
	cat.BytesDelivered = nil
	seed(t, store, cat)
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	got := map[string][]string{}
	for _, row := range table.Rows {
		got[row[0]] = row[1:3]
	}
	want := map[string][]string{x: {UnknownSize, UnknownSize}, y: {"100", "0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("READ BYTES, BASH BYTES = %v\nwant %v", got, want)
	}
	out := render(t, table)
	for _, note := range []string{
		"note: 2 Read calls have no delivered size: size unknown, not counted in READ BYTES",
		"note: 1 Bash calls have no delivered size: size unknown, not counted in BASH BYTES",
	} {
		if !strings.Contains(out, note) {
			t.Errorf("report lacks %q:\n%s", note, out)
		}
	}
}
