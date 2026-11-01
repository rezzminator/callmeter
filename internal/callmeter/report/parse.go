package report

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

// ParseBatchSize is how many Bash calls one cmdparse.ParseBatch (one python3
// process) takes.
const ParseBatchSize = 200

const statusOK = cmdparse.StatusOK

// ParseSummary counts what one EnsureParsed run did.
type ParseSummary struct {
	Parsed          int // calls whose parts were cached
	SkippedRelative int // calls left unparsed for a relative or missing cwd
	SkippedNoInput  int // calls left unparsed for an input without a readable command
}

// EnsureParsed parses every Bash call that has no command_parts rows from
// this parser (cmdparse.Version), a python-error part at every run, or a
// python-unavailable part only when python3 resolves at this run. It caches
// parts, replacing an older parser's; a parser error adds a parse fault with
// its message. home is where
// a leading `~` expands (runtime.Paths.Home: the commands ran as this user). A
// call whose cwd is not absolute stays unparsed (cmdparse refuses it) and is
// counted, as is one whose stored input is not JSON or carries no command.
func EnsureParsed(
	ctx context.Context,
	store *callmeter.Store,
	home string,
	runner cmdparse.PythonRunner,
) (ParseSummary, error) {
	var summary ParseSummary
	type pending struct {
		call      cmdparse.Call
		sessionID string
		ts        int64
	}
	var todo []pending
	retryUnavailable := cmdparse.PythonResolves(runner)
	retryStatuses := "?"
	args := []any{cmdparse.Version, cmdparse.StatusPythonError}
	if retryUnavailable {
		retryStatuses += ", ?"
		args = append(args, cmdparse.StatusPythonUnavailable)
	}
	err := query(ctx, store, "unparsed Bash calls",
		`SELECT c.tool_use_id, COALESCE(c.session_id, ''), COALESCE(c.ts, 0), COALESCE(c.input, ''), COALESCE(c.cwd, '')
		FROM calls c WHERE c.tool = 'Bash'
		AND (NOT EXISTS (SELECT 1 FROM command_parts p WHERE p.tool_use_id = c.tool_use_id AND p.parser = ?)
		OR EXISTS (SELECT 1 FROM command_parts p WHERE p.tool_use_id = c.tool_use_id AND p.parse_status IN (`+retryStatuses+`)))
		ORDER BY c.ts, c.tool_use_id`, args,
		func(r rowSource) error {
			var p pending
			var input string
			if err := r.Scan(&p.call.ID, &p.sessionID, &p.ts, &input, &p.call.Cwd); err != nil {
				return err
			}
			var fields struct {
				Command json.RawMessage `json:"command"`
			}
			// An input that is not JSON reads as one without a command: that
			// call stays unparsed and counted, never the run.
			badInput := input != "" && json.Unmarshal([]byte(input), &fields) != nil
			var command *string
			switch {
			case badInput || len(fields.Command) == 0 || json.Unmarshal(fields.Command, &command) != nil || command == nil:
				summary.SkippedNoInput++
			case !filepath.IsAbs(p.call.Cwd):
				summary.SkippedRelative++
			default:
				p.call.Command, p.call.Home = *command, home
				todo = append(todo, p)
			}
			return nil
		})
	if err != nil {
		return summary, err
	}
	for start := 0; start < len(todo); start += ParseBatchSize {
		batch := todo[start:min(start+ParseBatchSize, len(todo))]
		calls := make([]cmdparse.Call, len(batch))
		for i, p := range batch {
			calls[i] = p.call
		}
		parsed, err := cmdparse.ParseBatch(ctx, calls, runner)
		if err != nil {
			return summary, fmt.Errorf(
				"callmeter report: parse batch of %d calls from %s: %w",
				len(calls),
				calls[0].ID,
				err,
			)
		}
		for _, p := range batch {
			parts := toStoreParts(parsed[p.call.ID])
			if err := store.Batch(ctx, func(tx *callmeter.Tx) error {
				if err := tx.ReplaceCommandParts(ctx, p.call.ID, parts); err != nil {
					return fmt.Errorf("callmeter report: cache parts: %w", err)
				}
				for j := range parsed[p.call.ID] {
					part := &parsed[p.call.ID][j]
					if part.Status != cmdparse.StatusError {
						continue
					}
					if err := tx.AddFault(ctx, callmeter.Fault{
						TS:        p.ts,
						SessionID: p.sessionID,
						ToolUseID: p.call.ID,
						Stage:     callmeter.StageParse,
						Error:     callmeter.ParseFault(part.Seq, part.Program, part.Error),
					}); err != nil {
						return fmt.Errorf("callmeter report: record parse fault: %w", err)
					}
				}
				return nil
			}); err != nil {
				return summary, err
			}
			summary.Parsed++
		}
	}
	return summary, nil
}

// toStoreParts converts parsed parts to rows. A call that parsed to no part
// at all (bare assignments) is cached as one empty ok part, so it is never
// parsed again.
func toStoreParts(parts []cmdparse.Part) []callmeter.CommandPart {
	if len(parts) == 0 {
		return []callmeter.CommandPart{{Seq: 0, Lang: cmdparse.LangSh, ParseStatus: statusOK, Parser: cmdparse.Version}}
	}
	out := make([]callmeter.CommandPart, len(parts))
	for i := range parts {
		part := &parts[i]
		files := make([]string, len(part.Files))
		for j, ref := range part.Files {
			files[j] = EncodeFileRef(ref)
		}
		out[i] = callmeter.CommandPart{
			Seq: part.Seq, Lang: part.Lang, Program: part.Program,
			Args: part.Args, Files: files, ParseStatus: part.Status,
			Conditional: part.Conditional, Parser: cmdparse.Version,
		}
	}
	return out
}

// storedExists is each Existence's stored character: 0 missing, 1 present,
// ? not known (the parse-time stat failed).
var storedExists = map[cmdparse.Existence]string{
	cmdparse.ExistsNo: "0", cmdparse.ExistsYes: "1", cmdparse.ExistsUnknown: "?",
}

// EncodeFileRef is the stored form of a file reference:
// {action}\t{range}\t{exists 0|1|?}\t{path}. The path stays last, so a tab in
// a path survives.
func EncodeFileRef(ref cmdparse.FileRef) string {
	return ref.Action + "\t" + ref.Range + "\t" + storedExists[ref.Exists] + "\t" + ref.Path
}

// DecodeFileRef reads EncodeFileRef's form back; any other shape is an error
// naming the stored value.
func DecodeFileRef(s string) (cmdparse.FileRef, error) {
	fields := strings.SplitN(s, "\t", 4)
	if len(fields) == 4 {
		for exists, stored := range storedExists {
			if fields[2] == stored {
				return cmdparse.FileRef{Action: fields[0], Range: fields[1], Exists: exists, Path: fields[3]}, nil
			}
		}
	}
	return cmdparse.FileRef{}, fmt.Errorf(
		"callmeter report: stored file reference %q is not action\\trange\\texists\\tpath",
		s,
	)
}

// storedPart is one command_parts row with its call's facts.
type storedPart struct {
	callID      string
	program     string
	args        []string
	files       []cmdparse.FileRef
	conditional bool
}

// loadParts reads the parts of every call the filter keeps, in call then seq
// order, keyed by call id. A stored value that does not decode costs only
// itself: args that do not decode leave the part's args unknown (its shape is
// the program alone), a files list or a file reference that does not decode
// drops those files; every other part and call is kept, and the notes count
// each kind.
func loadParts(ctx context.Context, store *callmeter.Store, f Filter) (map[string][]storedPart, []string, error) {
	where, args := f.where()
	out := map[string][]storedPart{}
	var badArgs, badFiles, badRefs int64
	err := query(ctx, store, "command parts",
		`SELECT p.tool_use_id, COALESCE(p.program, ''), COALESCE(p.args, '[]'), COALESCE(p.files, '[]'), p.conditional
		FROM command_parts p JOIN calls c ON c.tool_use_id = p.tool_use_id
		WHERE `+where+` ORDER BY p.tool_use_id, p.seq`, args,
		func(r rowSource) error {
			var sp storedPart
			var argsJSON, filesJSON string
			if err := r.Scan(&sp.callID, &sp.program, &argsJSON, &filesJSON, &sp.conditional); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(argsJSON), &sp.args); err != nil {
				sp.args = nil
				badArgs++
			}
			var files []string
			if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
				files = nil
				badFiles++
			}
			for _, encoded := range files {
				ref, err := DecodeFileRef(encoded)
				if err != nil {
					badRefs++
					continue
				}
				sp.files = append(sp.files, ref)
			}
			out[sp.callID] = append(out[sp.callID], sp)
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	if badArgs > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d stored command parts have args that do not decode: args unknown, each shaped by its program alone", badArgs))
	}
	if badFiles > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d stored command parts have a files list that does not decode: their files are not counted", badFiles))
	}
	if badRefs > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d stored file references do not decode: those files are not counted", badRefs))
	}
	return out, notes, nil
}
