package callmeter

import (
	"math"
	"regexp"
	"strings"
)

// The price table below is Anthropic's published list in USD per million
// tokens, as fetched 2026-10-08 (https://platform.claude.com/docs/en/about-claude/pricing.md),
// and the OpenAI list of the models a Claude Code session can reach through a
// gateway. It prices a request whose cost Claude Code's own ledger did not give
// (cost_source 'table'): the request's own usage only, so a server-tool fee
// (a web search is billed per search, beyond its tokens) and an advisor
// iteration billed to another model are not in it. A model the table does not
// list is unpriced, never guessed. An OpenAI price is a list-equivalent: the
// route runs on a ChatGPT subscription, so no per-token bill exists.

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

// openAILongAbove is the context above which an OpenAI row takes its long
// tier: input, cache read and cache write tokens together, as the Claude rows
// count a context.
const openAILongAbove = 272000

// openAIRow is an OpenAI model's row. input_tokens is the uncached input and
// cache_read_tokens the cached part; there is no cache-write fee, so a written
// token bills as input (both write columns carry the input rate).
func openAIRow(key string, in, out, cached, longIn, longOut, longCached float64) priceRow {
	return priceRow{
		key:   key,
		rates: rates{in, out, cached, in, in},
		long:  &longTier{above: openAILongAbove, rates: rates{longIn, longOut, longCached, longIn, longIn}},
	}
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
	openAIRow("gpt-5.6-luna", 0.2, 1.2, 0.02, 0.4, 1.8, 0.04),
	openAIRow("gpt-5.6-sol", 4, 20, 0.4, 8, 30, 0.8),
	openAIRow("gpt-5.6-terra", 2, 12, 0.2, 4, 18, 0.4),
	openAIRow("gpt-6-astra", 10, 50, 1, 20, 75, 2),
	openAIRow("gpt-6-luna", 0.1, 0.5, 0.01, 0.2, 0.75, 0.02),
	openAIRow("gpt-6-sol", 2, 10, 0.2, 4, 15, 0.4),
	openAIRow("gpt-6.1-sol", 2, 10, 0.1, 4, 15, 0.2),
}

var (
	snapshotDate = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
	contextTag   = regexp.MustCompile(`\[[^\]]*\]$`)
)

// priceRowOf is the row that prices model: its id lower-cased, without a
// trailing context tag ("[1m]"), a Claude id with dots read as dashes (a GPT id
// keeps its dots: gpt-6.1-sol is the model's real id), else the same id
// without a trailing snapshot date.
func priceRowOf(model string) (priceRow, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	id = contextTag.ReplaceAllString(id, "")
	switch {
	case strings.HasPrefix(id, "claude-"):
		id = strings.ReplaceAll(id, ".", "-")
	case strings.HasPrefix(id, "gpt-"):
	default:
		return priceRow{}, false
	}
	for _, key := range []string{id, snapshotDate.ReplaceAllString(id, "")} {
		for _, row := range priceTable {
			if row.key == key {
				return row, true
			}
		}
	}
	return priceRow{}, false
}

// tierRates is the rates that price a request of the row: the long tier above
// its bound, else the base one, and the cache write split by lifetime. A write
// is split by lifetime when write5m or write1h is given; what the split leaves
// of cacheCreation, or all of it without a split, is the default 5-minute write.
func tierRates(row priceRow, input, cacheRead, cacheCreation int64, write5m, write1h *int64) (r rates, w5, w1 int64) {
	if write5m != nil {
		w5 = *write5m
	}
	if write1h != nil {
		w1 = *write1h
	}
	w5 += max(cacheCreation-w5-w1, 0)
	r = row.rates
	if row.long != nil && input+cacheRead+w5+w1 > row.long.above {
		r = row.long.rates
	}
	return r, w5, w1
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
	r, w5, w1 := tierRates(row, input, cacheRead, cacheCreation, write5m, write1h)
	total := float64(input)*r.input + float64(output)*r.output + float64(cacheRead)*r.cacheRead +
		float64(w5)*r.write5m + float64(w1)*r.write1h
	return int64(math.Round(total)), true
}

// WritePriceMicros is the average price of one cache-write token of a request,
// in USD micros per token (the table's USD per million tokens, as it stands):
// its 5-minute and 1-hour writes weighted by their counts, at the tier its
// context selects. A request that wrote nothing takes the default 5-minute
// rate. ok is false for a model the table does not list.
func WritePriceMicros(model string, input, cacheRead, cacheCreation int64, write5m, write1h *int64) (perToken float64, ok bool) {
	row, ok := priceRowOf(model)
	if !ok {
		return 0, false
	}
	r, w5, w1 := tierRates(row, input, cacheRead, cacheCreation, write5m, write1h)
	if w5+w1 == 0 {
		return r.write5m, true
	}
	return (float64(w5)*r.write5m + float64(w1)*r.write1h) / float64(w5+w1), true
}
