package report

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// The costly band: output just under the harness's 30,000-char persistence
// limit is delivered whole.
const (
	BandLow  = 20000
	BandHigh = 30000 // exclusive: output at or over it is persisted
)

// UnknownSize is a byte cell whose every contributing call has no delivered
// size: the size is unknown, never 0 bytes.
const UnknownSize = "unknown"

// sizeCell is a byte sum, or UnknownSize when none of the calls it covers had
// a delivered size: a sum of nothing known is unknown, never 0.
func sizeCell(sum, known, unknown int64) string {
	if known == 0 && unknown > 0 {
		return UnknownSize
	}
	return itoa(sum)
}

// unknownSizeNote names the calls a table left out of its byte columns for
// having no delivered size, in gapNotes' words; gapNotes names their reasons.
func unknownSizeNote(n int64, calls, columns string) string {
	return fmt.Sprintf("%d %s have no delivered size: size unknown, not counted in %s", n, calls, columns)
}

// MaxShapeParts is how many part shapes a call's shape keeps.
const MaxShapeParts = 3

// UnparsedShape names a Bash call with no cached parts.
const UnparsedShape = "(unparsed)"

// trivialPrograms never shape a call.
var trivialPrograms = map[string]bool{"cd": true, "export": true, "set": true}

// literalTrivialPrograms never shape a call when every argument is a literal
// and the part attributes no file: a separator `echo ---` is noise, an
// `echo "$X"` or `echo x > f` is not.
var literalTrivialPrograms = map[string]bool{"echo": true, "printf": true, "true": true, ":": true}

// expansionMarkers in an argument's stored text mean it was not a literal:
// the parser keeps an unresolved parameter, command or process substitution
// as its source.
const expansionMarkers = "$`"

// trivialPart reports whether a part never shapes a call: the shared rule of
// the commands and sequences topics.
func trivialPart(p storedPart) bool {
	base := filepath.Base(p.program)
	if trivialPrograms[base] {
		return true
	}
	if !literalTrivialPrograms[base] || len(p.files) > 0 {
		return false
	}
	for _, arg := range p.args {
		if strings.ContainsAny(arg, expansionMarkers) || strings.Contains(arg, "<(") || strings.Contains(arg, ">(") {
			return false
		}
	}
	return true
}

// streamFilters are dropped from a call's shape when they name no file and
// the call has another non-trivial part: `go test ./... | tail` is `go test`.
var streamFilters = map[string]bool{
	"head": true, "tail": true, "grep": true, "sed": true, "awk": true, "sort": true, "uniq": true,
	"wc": true, "cut": true, "tr": true, "tee": true, "cat": true, "jq": true, "less": true,
	"more": true, "column": true, "nl": true,
}

func pathLike(arg string) bool {
	return strings.Contains(arg, "/") || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~")
}

// partShape is the program's base name plus its first argument when that is
// neither a flag nor a path.
func partShape(program string, args []string) string {
	base := filepath.Base(program)
	if len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-") && !pathLike(args[0]) {
		return base + " " + args[0]
	}
	return base
}

func isFilter(p storedPart) bool {
	if !streamFilters[filepath.Base(p.program)] || len(p.files) > 0 {
		return false
	}
	for _, arg := range p.args {
		if pathLike(arg) {
			return false
		}
	}
	return true
}

// callShape is a call's shape: its non-trivial parts' shapes in order,
// deduplicated, joined with " ; ", at most MaxShapeParts.
func callShape(parts []storedPart) string {
	if len(parts) == 0 {
		return UnparsedShape
	}
	var kept []storedPart
	for _, p := range parts {
		if p.program != "" && !trivialPart(p) {
			kept = append(kept, p)
		}
	}
	var primary []storedPart
	for _, p := range kept {
		if !isFilter(p) {
			primary = append(primary, p)
		}
	}
	if len(primary) > 0 {
		kept = primary
	}
	if len(kept) == 0 {
		for _, p := range parts {
			if p.program != "" {
				return partShape(p.program, p.args)
			}
		}
		return "(none)"
	}
	var shapes []string
	seen := map[string]bool{}
	for _, p := range kept {
		s := partShape(p.program, p.args)
		if seen[s] {
			continue
		}
		seen[s] = true
		shapes = append(shapes, s)
		if len(shapes) == MaxShapeParts {
			break
		}
	}
	return strings.Join(shapes, " ; ")
}

type shapeStat struct {
	shape     string
	calls     int64
	delivered []int64 // the known sizes: a call with none is only counted
	sum       int64
	band      int64
	persisted int64
	followUps int64
}

// percentile is the nearest-rank p-th percentile of sorted values.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}

// Commands ranks Bash command shapes by bytes delivered.
func Commands(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	parts, err := loadParts(ctx, store, f)
	if err != nil {
		return nil, err
	}
	stats := map[string]*shapeStat{}
	var unknown int64 // Bash calls with no delivered size
	where, args := f.where()
	err = query(ctx, store, "Bash calls",
		`SELECT c.tool_use_id, c.bytes_delivered, COALESCE(c.bytes_real, c.bytes_delivered),
		COALESCE(c.persisted_path, ''),
		(SELECT COUNT(*) FROM calls r WHERE r.tool = 'Read' AND c.persisted_path IS NOT NULL
			AND r.file_path = c.persisted_path AND r.file_path LIKE '%/tool-results/%'
			AND r.session_id IS c.session_id AND r.agent_id IS c.agent_id AND r.ts >= c.ts)
		FROM calls c WHERE c.tool = 'Bash' AND `+where, args,
		func(r rowSource) error {
			var id, persisted string
			var delivered, realBytes *int64
			var followUps int64
			if err := r.Scan(&id, &delivered, &realBytes, &persisted, &followUps); err != nil {
				return err
			}
			key := callShape(parts[id])
			s, ok := stats[key]
			if !ok {
				s = &shapeStat{shape: key}
				stats[key] = s
			}
			s.calls++
			if delivered == nil {
				unknown++
			} else {
				s.delivered = append(s.delivered, *delivered)
				s.sum += *delivered
			}
			if realBytes != nil && *realBytes >= BandLow && *realBytes < BandHigh {
				s.band++
			}
			if persisted != "" {
				s.persisted++
			}
			s.followUps += followUps
			return nil
		})
	if err != nil {
		return nil, err
	}
	list := make([]*shapeStat, 0, len(stats))
	for _, s := range stats {
		sort.Slice(s.delivered, func(i, j int) bool { return s.delivered[i] < s.delivered[j] })
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].sum != list[j].sum {
			return list[i].sum > list[j].sum
		}
		return list[i].shape < list[j].shape
	})
	t := &Table{
		Title:  f.title("commands", n),
		Header: []string{"SHAPE", "CALLS", "BYTES", "P50", "P95", "20K-30K", "PERSISTED", "FOLLOW-UP READS"},
	}
	for _, s := range list[:min(len(list), f.limit())] {
		known := int64(len(s.delivered))
		t.Rows = append(t.Rows, []string{
			s.shape, itoa(s.calls), sizeCell(s.sum, known, s.calls-known),
			sizeCell(percentile(s.delivered, 50), known, s.calls-known),
			sizeCell(percentile(s.delivered, 95), known, s.calls-known),
			itoa(s.band), itoa(s.persisted), itoa(s.followUps),
		})
	}
	notes, err := gapNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	t.Notes = notes
	if unknown > 0 {
		t.Notes = append(t.Notes, unknownSizeNote(unknown, "Bash calls", "BYTES, P50 or P95"))
	}
	t.Notes = append(t.Notes, n.notes()...)
	return t, nil
}
