package hookentry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The replay lab: one sanitized real-session fixture (testdata/verify,
// testdata/gym/{S}, written by scripts/sanitize-capture.py) fed through the
// hook in-process, each payload's clock reading fixed by its index, and every
// table dumped as sorted rows for comparison.

// replayEpoch is the clock reading of payload 0; payload i lands replayStep
// ms after payload i-1.
const (
	replayEpoch = int64(1790000000000)
	replayStep  = int64(1000)
)

// gymSessions are the gym capture's sessions, in capture order.
var gymSessions = []string{"S1", "S1b", "S2", "S3", "S4"}

type replay struct {
	t        *testing.T
	lab      *callmeterLab
	fixture  string   // the directory under testdata
	payloads []string // the payloads callmeter receives, rewritten to the lab
}

// newReplay is a lab whose home is the fixture's home and whose payloads are
// the fixture's, minus the events the plugin does not register: Claude Code
// never hands those to callmeter.
func newReplay(t *testing.T, fixture string) *replay {
	t.Helper()
	lab := newCallmeterLab(t)
	if err := os.RemoveAll(lab.home); err != nil {
		t.Fatalf("clear the lab home: %v", err)
	}
	if err := os.CopyFS(lab.home, os.DirFS(filepath.Join("testdata", fixture, "home"))); err != nil {
		t.Fatalf("copy the %s home: %v", fixture, err)
	}
	registered := registeredEvents(t)
	r := &replay{t: t, lab: lab, fixture: fixture}
	for _, line := range fixturePayloads(t, fixture) {
		if registered[eventName(t, line)] {
			r.payloads = append(r.payloads, lab.rewrite(line))
		}
	}
	if len(r.payloads) == 0 {
		t.Fatalf("fixture %s holds no registered payload", fixture)
	}
	return r
}

// fixturePayloads are the lines of testdata/{fixture}/payloads.jsonl as
// committed, neutral roots unrewritten.
func fixturePayloads(t testing.TB, fixture string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture, "payloads.jsonl"))
	if err != nil {
		t.Fatalf("read %s payloads: %v", fixture, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// registeredEvents are the hook events the plugin's hooks.json registers.
func registeredEvents(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "plugins", "callmeter", "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var config struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode hooks.json: %v", err)
	}
	if len(config.Hooks) == 0 {
		t.Fatal("hooks.json registers no event")
	}
	events := map[string]bool{}
	for name := range config.Hooks {
		events[name] = true
	}
	return events
}

// decoded is one payload as a generic JSON object.
func decoded(t *testing.T, payload string) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return fields
}

func eventName(t *testing.T, payload string) string {
	t.Helper()
	name, _ := decoded(t, payload)["hook_event_name"].(string)
	return name
}

// feedIndex feeds one payload at the clock reading of capture index i.
func (r *replay) feedIndex(i int, payload string) {
	r.t.Helper()
	r.lab.feedAt(replayEpoch+int64(i)*replayStep, payload)
}

// feedInOrder feeds every payload in capture order.
func (r *replay) feedInOrder() {
	r.t.Helper()
	for i, payload := range r.payloads {
		r.feedIndex(i, payload)
	}
}

// feedOrder feeds the payloads in order, each at its own capture index's time.
func (r *replay) feedOrder(order []int) {
	r.t.Helper()
	for _, i := range order {
		r.feedIndex(i, r.payloads[i])
	}
}

// matching are the indexes of the payloads of event whose fields hold want.
func (r *replay) matching(event string, want func(map[string]any) bool) []int {
	r.t.Helper()
	var found []int
	for i, payload := range r.payloads {
		fields := decoded(r.t, payload)
		if fields["hook_event_name"] == event && (want == nil || want(fields)) {
			found = append(found, i)
		}
	}
	return found
}

// faults are the stored faults as "stage: error" lines.
func (r *replay) faults() []string {
	r.t.Helper()
	return r.lab.dump()["faults"]
}

// normalized is the lab's dump with what differs between two labs by design
// written neutrally: the lab's temp root, and each payload's event_id (the
// hash of its bytes, which carry that root) as its capture index. Nothing
// order-dependent is touched.
func (r *replay) normalized() map[string][]string {
	r.t.Helper()
	names := []string{r.lab.root}
	if resolved, err := filepath.EvalSymlinks(r.lab.root); err == nil && resolved != r.lab.root {
		names = append(names, resolved)
	}
	pairs := []string{}
	for _, name := range names {
		pairs = append(pairs, name, "{lab}")
	}
	for i, payload := range r.payloads {
		pairs = append(pairs, eventIDOf(payload), fmt.Sprintf("{event %d}", i))
	}
	neutral := strings.NewReplacer(pairs...)
	out := map[string][]string{}
	for table, rows := range r.lab.dump() {
		for _, row := range rows {
			out[table] = append(out[table], neutral.Replace(row))
		}
		slices.Sort(out[table])
	}
	return out
}

// dump is every table of the store as rows, each row its columns joined and
// the rows sorted: two stores holding the same set of rows dump equal.
func (lab *callmeterLab) dump() map[string][]string {
	lab.t.Helper()
	db := lab.db().DB()
	tables, err := db.QueryContext(lab.ctx, "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		lab.t.Fatalf("list tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			lab.t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := errors.Join(tables.Err(), tables.Close()); err != nil {
		lab.t.Fatalf("read table names: %v", err)
	}
	// Collect every table's rows before the next query: the store holds one
	// connection.
	out := map[string][]string{}
	for _, name := range names {
		rows, err := db.QueryContext(lab.ctx, "SELECT * FROM "+name)
		if err != nil {
			lab.t.Fatalf("read %s: %v", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			lab.t.Fatalf("columns of %s: %v", name, err)
		}
		lines := []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				lab.t.Fatalf("scan %s: %v", name, err)
			}
			cells := make([]string, len(values))
			for i, value := range values {
				if text, ok := value.([]byte); ok {
					value = string(text)
				}
				cells[i] = fmt.Sprintf("%s=%#v", columns[i], value)
			}
			lines = append(lines, strings.Join(cells, " | "))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			lab.t.Fatalf("read rows of %s: %v", name, err)
		}
		slices.Sort(lines)
		out[name] = lines
	}
	return out
}

// sameDump fails the test with every table whose rows differ.
func sameDump(t *testing.T, what string, want, got map[string][]string) {
	t.Helper()
	for table := range want {
		if _, ok := got[table]; !ok {
			t.Errorf("%s: table %s is missing", what, table)
		}
	}
	for table, rows := range got {
		wantRows := want[table]
		if slices.Equal(wantRows, rows) {
			continue
		}
		var only []string
		for _, row := range rows {
			if !slices.Contains(wantRows, row) {
				only = append(only, "  + "+row)
			}
		}
		for _, row := range wantRows {
			if !slices.Contains(rows, row) {
				only = append(only, "  - "+row)
			}
		}
		t.Errorf("%s: table %s differs from the in-order store (%d rows, want %d):\n%s",
			what, table, len(rows), len(wantRows), strings.Join(only, "\n"))
	}
}
