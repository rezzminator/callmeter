package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func TestFaultsCountsByStageAndStatusThenLatest(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for i, stage := range []string{callmeter.StagePayload, callmeter.StagePayload, callmeter.StageParse} {
		if err := store.AddFault(ctx, callmeter.Fault{
			TS: ms(time.Duration(3-i) * time.Hour), Stage: stage, Error: stage + " broke",
		}); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), "node -e 'x'", 5))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Faults(ctx, store, Filter{Limit: 2}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	counts := map[string]string{}
	var latest []string
	for _, row := range table.Rows {
		switch row[0] {
		case "stage", "parse":
			counts[row[0]+":"+row[1]] = row[2]
		case "fault":
			latest = append(latest, row[6])
		}
	}
	for key, want := range map[string]string{"stage:payload": "2", "stage:parse": "1", "stage:store": "0", "stage:binary": "0", "parse:unparsed": "1"} {
		if counts[key] != want {
			t.Errorf("count %s = %q, want %s (rows %v)", key, counts[key], want, table.Rows)
		}
	}
	if len(latest) != 2 || latest[0] != "parse broke" {
		t.Errorf("latest faults = %v, want 2, newest first", latest)
	}
}

func TestFaultsEmptyWindow(t *testing.T) {
	table, err := Faults(context.Background(), openStore(t), Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("empty store rows = %v, want none", table.Rows)
	}
}

// A fault carrying a session resolves its chat name through a NameOf that
// queries the same one-connection store: the report must return, not wait on
// its own open scan.
func TestFaultsSessionChatNameFromSameStore(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), Stage: callmeter.StageStore, SessionID: "s1", Error: "store broke",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	nameOf := func(session string) (string, error) {
		var n int64
		if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM faults").Scan(&n); err != nil {
			return "", err
		}
		return "chat-" + session, nil
	}
	done := make(chan struct{})
	var table *Table
	var err error
	go func() {
		defer close(done)
		table, err = Faults(ctx, store, Filter{}, nameOf)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Faults did not return: the name lookup waits on the open scan")
	}
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	for _, row := range table.Rows {
		if row[0] == "fault" {
			if row[4] != "chat-s1" {
				t.Errorf("fault CHAT = %q, want chat-s1 (row %v)", row[4], row)
			}
			return
		}
	}
	t.Fatalf("no fault row in %v", table.Rows)
}

func TestFaultsNameOfErrorSurfacesAsNote(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), Stage: callmeter.StageStore, SessionID: "s1", Error: "store broke",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	table, err := Faults(ctx, store, Filter{}, func(string) (string, error) { return "", errNames })
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	if len(table.Notes) == 0 {
		t.Errorf("notes = none, want the NameOf error noted")
	}
	for _, row := range table.Rows {
		if row[0] == "fault" && row[4] != "?" {
			t.Errorf("fault CHAT = %q, want ?", row[4])
		}
	}
}

func TestFaultsBinaryStageListedNotUnknown(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for i := 0; i < 2; i++ {
		if err := store.AddFault(ctx, callmeter.Fault{
			TS: ms(time.Duration(i+1) * time.Hour), Stage: callmeter.StageBinary, Error: "binary missing",
		}); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	table, err := Faults(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var stageRows []string
	for _, row := range table.Rows {
		if row[0] != "stage" {
			continue
		}
		stageRows = append(stageRows, row[1]+"="+row[2])
	}
	want := []string{"payload=0", "store=0", "transcript=0", "parse=0", "binary=2"}
	if strings.Join(stageRows, ",") != strings.Join(want, ",") {
		t.Errorf("stage rows = %v, want %v", stageRows, want)
	}
}

func TestFaultsStageListWithNoFaults(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), "ls", 5))
	table, err := Faults(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var stageRows []string
	for _, row := range table.Rows {
		if row[0] == "stage" {
			stageRows = append(stageRows, row[1]+"="+row[2])
		}
	}
	want := []string{"payload=0", "store=0", "transcript=0", "parse=0", "binary=0"}
	if strings.Join(stageRows, ",") != strings.Join(want, ",") {
		t.Errorf("stage rows = %v, want %v", stageRows, want)
	}
}
