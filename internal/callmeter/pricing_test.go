package callmeter

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// sampleRow is a row of testdata/ledger/sample-15.jsonl, the cost ledger's
// contract sample with invented ids.
type sampleRow struct {
	RequestID     string `json:"request_id"`
	TS            string `json:"ts"`
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	ParentAgentID string `json:"parent_agent_id"`
	QuerySource   string `json:"query_source"`
	Model         string `json:"model"`
	Input         int64  `json:"input_tokens"`
	Output        int64  `json:"output_tokens"`
	CacheRead     int64  `json:"cache_read_tokens"`
	CacheCreation int64  `json:"cache_creation_tokens"`
	Cost          *int64 `json:"cost_usd_micros"`
	Join          string `json:"join"`
}

func readSample(t *testing.T) []sampleRow {
	t.Helper()
	file, err := os.Open("testdata/ledger/sample-15.jsonl")
	if err != nil {
		t.Fatalf("open the ledger sample: %v", err)
	}
	defer file.Close()
	var rows []sampleRow
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row sampleRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("parse a ledger sample line: %v", err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the ledger sample: %v", err)
	}
	return rows
}

// TestPriceMicrosReproducesTheLedgerSample prices each sample row from its
// tokens. Claude Code's ledger carries no 5m/1h split, so the split is the one
// that reproduces the row (the cache lifetime that call used); rows 7 and 8
// are web searches, whose price adds the per-search fee that no token count
// carries, so the table is exactly 10000 micros short of the ledger.
func TestPriceMicrosReproducesTheLedgerSample(t *testing.T) {
	oneHour := map[string]bool{"01": true, "02": true, "15": true}
	rows := readSample(t)
	if len(rows) != 15 {
		t.Fatalf("sample rows = %d, want 15", len(rows))
	}
	for _, row := range rows {
		id := row.RequestID[len(row.RequestID)-2:]
		var w5, w1 int64
		if oneHour[id] {
			w1 = row.CacheCreation
		} else {
			w5 = row.CacheCreation
		}
		got, ok := PriceMicros(row.Model, row.Input, row.Output, row.CacheRead, row.CacheCreation, &w5, &w1)
		if !ok {
			t.Errorf("row %s: %s has no table price", id, row.Model)
			continue
		}
		want := *row.Cost
		if row.QuerySource == "web_search_tool" {
			want -= 10000
		}
		if got != want {
			t.Errorf("row %s (%s %s): price = %d, want %d", id, row.Model, row.QuerySource, got, want)
		}
	}
}

func ptr(n int64) *int64 { return &n }

func TestPriceMicrosTiersSplitsAndModelNames(t *testing.T) {
	cases := []struct {
		name                  string
		model                 string
		in, out, read, create int64
		w5, w1                *int64
		want                  int64
		wantOK                bool
	}{
		// 100000 context is the tier's edge: not above it.
		{"haiku 5.5 at the tier edge", "claude-haiku-5-5", 1000, 100, 99000, 0, nil, nil, 100 + 50 + 990, true},
		// 100001 context: every count at the long rates (0.5, 2.5, 0.05).
		{"haiku 5.5 above the tier", "claude-haiku-5-5", 1000, 100, 99001, 0, nil, nil, 500 + 250 + 4950, true},
		{"split writes", "claude-opus-5-5", 0, 0, 0, 30, ptr(10), ptr(20), 10*5 + 20*8, true},
		{"unsplit write is 5m", "claude-opus-5-5", 0, 0, 0, 30, nil, nil, 30 * 5, true},
		{"short split leaves the rest at 5m", "claude-opus-5-5", 0, 0, 0, 30, ptr(5), ptr(10), 20*5 + 10*8, true},
		{"snapshot date", "claude-haiku-4-5-20251001", 10, 5, 100, 100, nil, nil, 10*1 + 5*5 + 100*0.1 + 100*1.25, true},
		{"context suffix", "claude-opus-5-5[1m]", 10, 0, 0, 0, nil, nil, 40, true},
		{"unknown model", "claude-nonesuch-9", 10, 5, 0, 0, nil, nil, 0, false},
		{"synthetic", "<synthetic>", 10, 5, 0, 0, nil, nil, 0, false},
		{"empty", "", 10, 5, 0, 0, nil, nil, 0, false},
	}
	for _, c := range cases {
		got, ok := PriceMicros(c.model, c.in, c.out, c.read, c.create, c.w5, c.w1)
		if ok != c.wantOK || got != c.want {
			t.Errorf("%s: PriceMicros = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
