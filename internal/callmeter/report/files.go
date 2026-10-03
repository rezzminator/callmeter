package report

import (
	"context"
	"fmt"
	"sort"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

// readActions are the attributed actions that deliver a file's content (or
// part of it) to the model. Write and exec do not; unknown (a wrapper or a
// program the parser does not know) may or may not, so it counts nowhere.
var readActions = map[string]bool{
	cmdparse.ActionReadWhole: true,
	cmdparse.ActionReadRange: true,
	cmdparse.ActionSearch:    true,
}

// writeTools are the tools whose result gives a file's size before and after.
var writeTools = []any{"Write", "Edit", "MultiEdit", "NotebookEdit"}

type fileStat struct {
	path      string
	readBytes int64 // via the Read tool
	bashBytes int64 // the bytes of Bash calls reading this one file and running none
	reads     int64
	certain   int64            // reads a part outside every branch made
	cond      int64            // reads made only by parts inside a branch
	agents    map[string]int64 // reads per agent
	size      *int64           // last size on disk seen by a Read
	gone      bool             // the latest look found no file: a Bash reference with Exists false
	sizeTS    int64            // ts of the latest look: a Read's size or a Bash reference's existence
	whole     int64
	ranged    int64
	execs     int64 // Bash calls running the file
	// Calls behind each byte column with and without a delivered size: one
	// with none is left out of the sum, never added as 0. bashShared counts
	// reads whose call's bytes went to no file, since the call read several
	// files or also ran one.
	readKnown, readUnknown, bashKnown, bashUnknown, bashShared int64
}

func agentKey(session, agent string) string { return session + "/" + agent }

// Files ranks files by bytes delivered: Read bytes and Bash-attributed bytes
// are two columns, never summed; rows rank by the larger of the two. Only a
// read mode (read-whole, read-range, search) is a Bash read. A Bash call's
// bytes go to a file only when the call reads that one file and runs no file
// (exec) and names none through a wrapper (unknown): its output cannot be
// split per file otherwise, so such a call's reads count, its bytes go to no
// file, and a note names the calls. A Bash call running a file counts in
// EXECS alone; an unknown attribution counts nowhere and a note names how
// many there were. READS splits into CERTAIN
// and CONDITIONAL: a Read call, or a Bash call with a part outside every
// branch naming the file, is certain; a Bash call naming it only in parts
// inside a branch, a case arm or right of && / || is conditional. A file the
// parse found missing counts like any other; its SIZE is "gone" when the
// latest call naming it found no file, never a 0 that reads as empty.
func Files(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	stats := map[string]*fileStat{}
	stat := func(path string) *fileStat {
		s, ok := stats[path]
		if !ok {
			s = &fileStat{path: path, agents: map[string]int64{}}
			stats[path] = s
		}
		return s
	}
	var readUnknown, bashUnknown int64 // calls with no delivered size
	var bashShared, unknownRefs int64  // calls whose bytes go to no file; unknown attributions
	where, args := f.where()
	err := query(ctx, store, "Read calls",
		`SELECT c.file_path, COALESCE(c.session_id, ''), COALESCE(c.agent_id, ''), COALESCE(c.ts, 0),
		c.bytes_delivered, c.file_bytes, c.read_start, c.read_lines, c.read_total_lines
		FROM calls c WHERE c.tool = 'Read' AND c.file_path IS NOT NULL AND `+where+` ORDER BY c.ts`, args,
		func(r rowSource) error {
			var path, session, agent string
			var ts int64
			var delivered, size, start, lines, total *int64
			if err := r.Scan(&path, &session, &agent, &ts, &delivered, &size, &start, &lines, &total); err != nil {
				return err
			}
			s := stat(path)
			if delivered == nil {
				s.readUnknown++
				readUnknown++
			} else {
				s.readBytes += *delivered
				s.readKnown++
			}
			s.reads++
			s.certain++
			s.agents[agentKey(session, agent)]++
			if size != nil && ts >= s.sizeTS {
				s.size, s.gone, s.sizeTS = size, false, ts
			}
			if (start != nil && *start > 1) || (lines != nil && total != nil && *lines < *total) {
				s.ranged++
			} else {
				s.whole++
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	parts, err := loadParts(ctx, store, f)
	if err != nil {
		return nil, err
	}
	err = query(ctx, store, "Bash calls",
		`SELECT c.tool_use_id, COALESCE(c.session_id, ''), COALESCE(c.agent_id, ''), COALESCE(c.ts, 0),
		c.bytes_delivered
		FROM calls c WHERE c.tool = 'Bash' AND `+where+` ORDER BY c.ts`, args,
		func(r rowSource) error {
			var id, session, agent string
			var ts int64
			var delivered *int64
			if err := r.Scan(&id, &session, &agent, &ts, &delivered); err != nil {
				return err
			}
			seen := map[string]bool{}
			var credited []string        // distinct read paths, in part order
			certain := map[string]bool{} // a certain part read it: the call's read is certain
			execs := map[string]bool{}   // paths the call runs
			opaque := false              // the call runs a file or names one through a wrapper
			for _, part := range parts[id] {
				for _, ref := range part.files {
					switch {
					case ref.Action == cmdparse.ActionExec:
						opaque = true
						if !execs[ref.Path] {
							execs[ref.Path] = true
							stat(ref.Path).execs++
						}
						continue
					case ref.Action == cmdparse.ActionUnknown:
						opaque = true
						unknownRefs++
						continue
					case !readActions[ref.Action]:
						continue
					}
					if !part.conditional {
						certain[ref.Path] = true
					}
					if !seen[ref.Path] {
						seen[ref.Path] = true
						credited = append(credited, ref.Path)
					}
				}
			}
			single := len(credited) == 1 && !opaque
			switch {
			case len(credited) == 0:
			case delivered == nil:
				bashUnknown++
			case !single:
				bashShared++
			}
			seen = map[string]bool{}
			for _, part := range parts[id] {
				for _, ref := range part.files {
					if !readActions[ref.Action] || seen[ref.Path] {
						continue
					}
					seen[ref.Path] = true
					s := stat(ref.Path)
					if ts >= s.sizeTS {
						// One call's references share one parse-time stat.
						s.gone, s.sizeTS = !ref.Exists, ts
					}
					switch {
					case delivered == nil:
						s.bashUnknown++
					case single:
						s.bashBytes += *delivered
						s.bashKnown++
					default:
						s.bashShared++
					}
					s.reads++
					if certain[ref.Path] {
						s.certain++
					} else {
						s.cond++
					}
					s.agents[agentKey(session, agent)]++
					switch ref.Action {
					case cmdparse.ActionReadWhole:
						s.whole++
					case cmdparse.ActionReadRange:
						s.ranged++
					}
				}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	list := make([]*fileStat, 0, len(stats))
	for _, s := range stats {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ka, kb := max(a.readBytes, a.bashBytes), max(b.readBytes, b.bashBytes); ka != kb {
			return ka > kb
		}
		if a.reads != b.reads {
			return a.reads > b.reads
		}
		return a.path < b.path
	})
	t := &Table{
		Title: f.title("files", n),
		Header: []string{
			"FILE",
			"READ BYTES",
			"BASH BYTES",
			"READS",
			"CERTAIN",
			"CONDITIONAL",
			"AGENTS",
			"SIZE",
			"WHOLE",
			"RANGED",
			"RE-READS",
			"EXECS",
		},
	}
	for _, s := range list[:min(len(list), f.limit())] {
		var rereads int64
		for _, count := range s.agents {
			rereads += count - 1
		}
		size := "-"
		switch {
		case s.gone:
			size = "gone"
		case s.size != nil:
			size = itoa(*s.size)
		}
		t.Rows = append(t.Rows, []string{
			s.path,
			sizeCell(s.readBytes, s.readKnown, s.readUnknown),
			sizeCell(s.bashBytes, s.bashKnown, s.bashUnknown+s.bashShared),
			itoa(s.reads),
			itoa(s.certain),
			itoa(s.cond),
			itoa(int64(len(s.agents))),
			size,
			itoa(s.whole),
			itoa(s.ranged),
			itoa(rereads),
			itoa(s.execs),
		})
	}
	notes, err := gapNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	t.Notes = notes
	if readUnknown > 0 {
		t.Notes = append(t.Notes, unknownSizeNote(readUnknown, "Read calls", "READ BYTES"))
	}
	if bashUnknown > 0 {
		t.Notes = append(t.Notes, unknownSizeNote(bashUnknown, "Bash calls", "BASH BYTES"))
	}
	if bashShared > 0 {
		t.Notes = append(t.Notes, sharedBytesNote(bashShared))
	}
	if unknownRefs > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d file attributions with mode unknown (a wrapper or unrecognised program): not counted", unknownRefs))
	}
	t.Notes = append(t.Notes, n.notes()...)
	return t, nil
}

type writeStat struct {
	path    string
	tool    int64
	bash    int64
	growth  int64
	unknown int64 // tool writes with a size missing, plus every Bash write
}

// Writes ranks files by total growth (file_bytes - file_bytes_before); a Bash
// write is counted but its growth is unknown.
func Writes(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	stats := map[string]*writeStat{}
	stat := func(path string) *writeStat {
		s, ok := stats[path]
		if !ok {
			s = &writeStat{path: path}
			stats[path] = s
		}
		return s
	}
	where, args := f.where()
	err := query(ctx, store, "write calls",
		`SELECT c.file_path, c.file_bytes, c.file_bytes_before FROM calls c
		WHERE c.tool IN (?, ?, ?, ?) AND c.file_path IS NOT NULL AND `+where, append(writeTools, args...),
		func(r rowSource) error {
			var path string
			var after, before *int64
			if err := r.Scan(&path, &after, &before); err != nil {
				return err
			}
			s := stat(path)
			s.tool++
			if after != nil && before != nil {
				s.growth += *after - *before
			} else {
				s.unknown++
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	parts, err := loadParts(ctx, store, f)
	if err != nil {
		return nil, err
	}
	for _, callParts := range parts {
		seen := map[string]bool{}
		for _, part := range callParts {
			for _, ref := range part.files {
				if ref.Action != cmdparse.ActionWrite || seen[ref.Path] {
					continue
				}
				seen[ref.Path] = true
				s := stat(ref.Path)
				s.bash++
				s.unknown++
			}
		}
	}
	list := make([]*writeStat, 0, len(stats))
	for _, s := range stats {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.growth != b.growth {
			return a.growth > b.growth
		}
		if a.tool+a.bash != b.tool+b.bash {
			return a.tool+a.bash > b.tool+b.bash
		}
		return a.path < b.path
	})
	t := &Table{
		Title:  f.title("writes", n),
		Header: []string{"FILE", "WRITES", "TOOL", "BASH", "GROWTH", "GROWTH UNKNOWN"},
	}
	for _, s := range list[:min(len(list), f.limit())] {
		t.Rows = append(t.Rows, []string{
			s.path, itoa(s.tool + s.bash), itoa(s.tool), itoa(s.bash), itoa(s.growth), itoa(s.unknown),
		})
	}
	notes, err := gapNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	t.Notes = notes
	t.Notes = append(t.Notes, n.notes()...)
	return t, nil
}

// sharedBytesNote names the Bash calls whose bytes BASH BYTES credits to no
// file: calls reading several files, or also running one.
func sharedBytesNote(n int64) string {
	return fmt.Sprintf("%d Bash calls read several files or also ran one: their bytes are credited to no file in BASH BYTES", n)
}
