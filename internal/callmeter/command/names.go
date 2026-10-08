package command

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/rezzminator/callmeter/internal/paths"
)

// idLen is how many characters of a session id name a chat that has no title.
const idLen = 8

// titleMarkers are the byte strings a transcript line holds when it is a title
// entry; every other line is skipped without being decoded.
var titleMarkers = [][]byte{
	[]byte(`"type":"custom-title"`), []byte(`"type":"ai-title"`), []byte(`"type":"summary"`),
}

// titleEntry is the part of a transcript title line the lookup reads.
type titleEntry struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	Summary     string `json:"summary"`
}

// titles are the last title of each kind a transcript holds.
type titles struct{ custom, ai, summary string }

// transcriptNames resolves a session id to its chat name from the session's
// transcript, read-only: the sessions.transcript_path the store recorded for
// it first, then {seat}/projects/*/{session_id}.jsonl over every seat the store
// recorded for the session and the seat the report itself runs under. The
// answer is the last customTitle, else the last aiTitle, else the
// last summary, else the session id's first characters. A transcript that is
// absent names nothing; one that exists but cannot be read returns its error,
// so the table prints "?" and a count with a safe label in its note; the full
// error, including the path, is written to stderr once per session.
type transcriptNames struct {
	ctx    context.Context
	db     *sql.DB
	getenv paths.Getenv
	stderr io.Writer
	told   map[string]bool
}

func (names *transcriptNames) nameOf(sessionID string) (name string, err error) {
	defer func() {
		if err == nil || names.stderr == nil {
			return
		}
		if names.told == nil {
			names.told = make(map[string]bool)
		}
		if names.told[sessionID] {
			return
		}
		names.told[sessionID] = true
		fmt.Fprintf(names.stderr, "callmeter: chat name: session %s: %v\n", sessionID, err)
	}()
	fallback := sessionID
	if runes := []rune(sessionID); len(runes) > idLen {
		fallback = string(runes[:idLen])
	}
	if !plainName(sessionID) {
		return fallback, nil
	}
	stored, err := names.storedTitle(sessionID)
	if err != nil {
		return "", err
	}
	if stored != "" {
		return stored, nil
	}
	seats, err := names.seats(sessionID)
	if err != nil {
		return "", err
	}
	for _, seat := range seats {
		found, err := titlesOf(seat, sessionID)
		if err != nil {
			return "", err
		}
		if title := found.best(); title != "" {
			return title, nil
		}
	}
	return fallback, nil
}

// storedTitle is the best title of the transcript the sessions table recorded
// for the session, "" when no path was recorded, the file is gone or holds no
// title. A path that exists but is not a regular file, or cannot be read, is
// an error: the transcript is there and its name is not known.
func (names *transcriptNames) storedTitle(sessionID string) (string, error) {
	var path sql.NullString
	err := names.db.QueryRowContext(names.ctx,
		"SELECT transcript_path FROM sessions WHERE session_id = ?", sessionID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!path.Valid || path.String == "")) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the transcript path of session %s: %w", sessionID, err)
	}
	if !filepath.IsAbs(path.String) {
		return "", nil
	}
	info, err := os.Stat(path.String)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat transcript %s: %w", path.String, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("transcript %s is not a regular file", path.String)
	}
	found, err := readTitles(path.String)
	if err != nil {
		return "", err
	}
	return found.best(), nil
}

// plainName is whether id can be one file name: a session id that carries a
// path separator or a dot-dot never names a transcript.
func plainName(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

// seats are the Claude Code config dirs to look under, in order: the distinct
// seat_dir values the store holds for the session, then the report's own
// seat. A seat that cannot be resolved is an error only when no stored seat
// exists to look under.
func (names *transcriptNames) seats(sessionID string) ([]string, error) {
	rows, err := names.db.QueryContext(names.ctx, `
		SELECT seat_dir FROM calls WHERE session_id = ?1 AND seat_dir IS NOT NULL AND seat_dir != ''
		UNION SELECT seat_dir FROM requests WHERE session_id = ?1 AND seat_dir IS NOT NULL AND seat_dir != ''
		UNION SELECT seat_dir FROM agents WHERE session_id = ?1 AND seat_dir IS NOT NULL AND seat_dir != ''
		ORDER BY seat_dir`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read the seats of session %s: %w", sessionID, err)
	}
	var seats []string
	for rows.Next() {
		var seat string
		if err := rows.Scan(&seat); err != nil {
			return nil, fmt.Errorf("read the seats of session %s: %w", sessionID, errors.Join(err, rows.Close()))
		}
		seats = append(seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the seats of session %s: %w", sessionID, errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read the seats of session %s: %w", sessionID, err)
	}
	own, err := paths.SeatDir(names.getenv)
	if err != nil {
		if len(seats) == 0 {
			return nil, fmt.Errorf("resolve the seat to look for chat names: %w", err)
		}
		return seats, nil
	}
	for _, seat := range seats {
		if seat == own {
			return seats, nil
		}
	}
	return append(seats, own), nil
}

// titlesOf reads the titles of session's transcript under seat: the first
// {seat}/projects/{project}/{session}.jsonl in project-name order. A seat with
// no projects directory or no such transcript holds none.
func titlesOf(seat, session string) (titles, error) {
	projects := filepath.Join(seat, "projects")
	entries, err := os.ReadDir(projects)
	if errors.Is(err, fs.ErrNotExist) {
		return titles{}, nil
	}
	if err != nil {
		return titles{}, fmt.Errorf("list transcripts under %s: %w", projects, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(projects, entry.Name(), session+".jsonl")
		found, err := readTitles(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return titles{}, err
		}
		return found, nil
	}
	return titles{}, nil
}

// readTitles scans one transcript for its title entries. A transcript that is
// not there returns an error wrapping fs.ErrNotExist.
func readTitles(path string) (found titles, err error) {
	file, err := os.Open(path)
	if err != nil {
		return titles{}, fmt.Errorf("open transcript %s: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close transcript %s: %w", path, closeErr)
		}
	}()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if hasTitleMarker(line) {
			var entry titleEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				return titles{}, fmt.Errorf("decode a title entry in transcript %s: %w", path, err)
			}
			found.add(entry)
		}
		if errors.Is(readErr, io.EOF) {
			return found, nil
		}
		if readErr != nil {
			return titles{}, fmt.Errorf("read transcript %s: %w", path, readErr)
		}
	}
}

func hasTitleMarker(line []byte) bool {
	for _, marker := range titleMarkers {
		if bytes.Contains(line, marker) {
			return true
		}
	}
	return false
}

// add keeps the last title of each kind that has any text once cleaned.
func (found *titles) add(entry titleEntry) {
	switch entry.Type {
	case "custom-title":
		if title := cleanTitle(entry.CustomTitle); title != "" {
			found.custom = title
		}
	case "ai-title":
		if title := cleanTitle(entry.AITitle); title != "" {
			found.ai = title
		}
	case "summary":
		if title := cleanTitle(entry.Summary); title != "" {
			found.summary = title
		}
	}
}

// best is the customTitle, else the aiTitle, else the summary; "" when none.
func (found titles) best() string {
	for _, title := range []string{found.custom, found.ai, found.summary} {
		if title != "" {
			return title
		}
	}
	return ""
}

// cleanTitle is title on one line: every run of whitespace and control
// characters (a tab, a newline, a terminal escape) becomes one space, so a
// title never breaks a table row or reaches the terminal as a command.
func cleanTitle(title string) string {
	return strings.Join(strings.FieldsFunc(title, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}
