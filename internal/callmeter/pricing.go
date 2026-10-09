package callmeter

import (
	"math"
	"regexp"
	"strings"
)

// The price table below is Anthropic's published list in USD per million
// tokens, as fetched 2026-10-08 (https://platform.claude.com/docs/en/about-claude/pricing.md).
// It prices a request whose cost Claude Code's own ledger did not give
// (cost_source 'table'): the request's own usage only, so a server-tool fee
// (a web search is billed per search, beyond its tokens) and an advisor
// iteration billed to another model are not in it. A model the table does not
// list is unpriced, never guessed.

// rates are the USD per million tokens of one model, or of its long tier.
type rates struct {
	input, output, cacheRead, write5m, write1h float64
}

// longTier is the upper tier a call is priced at when its context (input,
// cache read and cache write tokens) is above the bound.
type longTier struct {
	above int64
	rates rates
}

type priceRow struct {
	key   string
	rates rates
	long  *longTier
}

var priceTable = []priceRow{
	{"claude-fable-5", rates{10, 50, 1, 12.5, 20}, nil},
	{"claude-fable-5-1", rates{10, 50, 0.25, 12.5, 20}, nil},
	{"claude-haiku-3-5", rates{0.8, 4, 0.08, 1, 1.6}, nil},
	{"claude-haiku-4-5", rates{1, 5, 0.1, 1.25, 2}, nil},
	{"claude-haiku-5-5", rates{0.1, 0.5, 0.01, 0.125, 0.2}, &longTier{above: 100000, rates: rates{0.5, 2.5, 0.05, 0.625, 1}}},
	{"claude-mythos-5", rates{10, 50, 1, 12.5, 20}, nil},
	{"claude-mythos-5-1", rates{10, 50, 0.25, 12.5, 20}, nil},
	{"claude-opus-4", rates{15, 75, 1.5, 18.75, 30}, nil},
	{"claude-opus-4-1", rates{15, 75, 1.5, 18.75, 30}, nil},
	{"claude-opus-4-5", rates{5, 25, 0.5, 6.25, 10}, nil},
	{"claude-opus-4-6", rates{5, 25, 0.5, 6.25, 10}, nil},
	{"claude-opus-4-7", rates{5, 25, 0.5, 6.25, 10}, nil},
	{"claude-opus-4-8", rates{5, 25, 0.5, 6.25, 10}, nil},
	{"claude-opus-5", rates{5, 25, 0.5, 6.25, 10}, nil},
	{"claude-opus-5-5", rates{4, 20, 0.2, 5, 8}, nil},
	{"claude-sonnet-4", rates{3, 15, 0.3, 3.75, 6}, nil},
	{"claude-sonnet-4-5", rates{3, 15, 0.3, 3.75, 6}, nil},
	{"claude-sonnet-4-6", rates{3, 15, 0.3, 3.75, 6}, nil},
	{"claude-sonnet-5", rates{2, 10, 0.2, 2.5, 4}, nil},
	{"claude-sonnet-5-5", rates{2, 10, 0.1, 2.5, 4}, nil},
}

var (
	snapshotDate = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
	contextTag   = regexp.MustCompile(`\[[^\]]*\]$`)
)

// priceRowOf is the row that prices model: its id lower-cased, without a
// trailing context tag ("[1m]"), with dots read as dashes, else the same id
// without a trailing snapshot date.
func priceRowOf(model string) (priceRow, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	id = contextTag.ReplaceAllString(id, "")
	if !strings.HasPrefix(id, "claude-") {
		return priceRow{}, false
	}
	id = strings.ReplaceAll(id, ".", "-")
	for _, key := range []string{id, snapshotDate.ReplaceAllString(id, "")} {
		for _, row := range priceTable {
			if row.key == key {
				return row, true
			}
		}
	}
	return priceRow{}, false
}

// PriceMicros prices one request in USD micros from the embedded table, ok
// false for a model the table does not list. A cache write is split by
// lifetime when write5m or write1h is given; what the split leaves of
// cacheCreation, or all of it without a split, is priced as the default
// 5-minute write.
func PriceMicros(model string, input, output, cacheRead, cacheCreation int64, write5m, write1h *int64) (micros int64, ok bool) {
	row, ok := priceRowOf(model)
	if !ok {
		return 0, false
	}
	var w5, w1 int64
	if write5m != nil {
		w5 = *write5m
	}
	if write1h != nil {
		w1 = *write1h
	}
	w5 += max(cacheCreation-w5-w1, 0)
	r := row.rates
	if row.long != nil && input+cacheRead+w5+w1 > row.long.above {
		r = row.long.rates
	}
	total := float64(input)*r.input + float64(output)*r.output + float64(cacheRead)*r.cacheRead +
		float64(w5)*r.write5m + float64(w1)*r.write1h
	return int64(math.Round(total)), true
}
