package report

import (
	"context"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Outcomes lists what the chats did, latest first, in one table: `edit` per
// session and file (lines added and removed), `commit` per session and sha
// (its branch), `test` per session and runner (runs that passed and failed). A
// cell a kind has no value for is "-". A test call with no result recorded is
// neither passed nor failed.
func Outcomes(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Title:  f.title("outcomes", n),
		Header: []string{"KIND", "CHAT", "SUBJECT", "BRANCH", "ADDED", "REMOVED", "PASSED", "FAILED", "LAST"},
	}
	where, args := f.where()
	// The rows are read in full before any chat name is: a name lookup reads
	// the store, and the store has one connection.
	type outcomeRow struct {
		kind                                 string
		session, subject, branch             *string
		added, removed, passed, failed, last *int64
	}
	var found []outcomeRow
	err := query(ctx, store, "outcomes",
		`SELECT kind, session_id, subject, branch, added, removed, passed, failed, last FROM (
			SELECT 'edit' AS kind, c.session_id AS session_id, c.file_path AS subject, NULL AS branch,
				SUM(c.lines_added) AS added, SUM(c.lines_removed) AS removed, NULL AS passed, NULL AS failed,
				MAX(c.ts) AS last
			FROM calls c WHERE (c.lines_added IS NOT NULL OR c.lines_removed IS NOT NULL) AND `+where+`
			GROUP BY c.session_id, c.file_path
			UNION ALL
			SELECT 'commit', c.session_id, c.commit_sha, MAX(c.commit_branch), NULL, NULL, NULL, NULL, MAX(c.ts)
			FROM calls c WHERE c.commit_sha IS NOT NULL AND `+where+`
			GROUP BY c.session_id, c.commit_sha
			UNION ALL
			SELECT 'test', c.session_id, c.test_runner, NULL, NULL, NULL,
				SUM(CASE WHEN c.failed = 0 THEN 1 ELSE 0 END), SUM(CASE WHEN c.failed = 1 THEN 1 ELSE 0 END), MAX(c.ts)
			FROM calls c WHERE c.test_runner IS NOT NULL AND `+where+`
			GROUP BY c.session_id, c.test_runner
		) ORDER BY last DESC, kind, subject, session_id LIMIT ?`,
		append(append(append(append([]any{}, args...), args...), args...), f.limit()),
		func(r rowSource) error {
			var row outcomeRow
			if err := r.Scan(
				&row.kind, &row.session, &row.subject, &row.branch, &row.added, &row.removed, &row.passed, &row.failed, &row.last,
			); err != nil {
				return err
			}
			found = append(found, row)
			return nil
		})
	if err != nil {
		return nil, err
	}
	for _, row := range found {
		chat := "-"
		if row.session != nil && *row.session != "" {
			chat = n.of(*row.session)
		}
		t.Rows = append(t.Rows, []string{
			row.kind, chat, textCell(row.subject), textCell(row.branch), intCell(row.added), intCell(row.removed),
			intCell(row.passed), intCell(row.failed), stampCell(row.last),
		})
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	return t, nil
}
