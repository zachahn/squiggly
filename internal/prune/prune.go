package prune

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zachahn/editor/internal/session"
)

var dayComponent = regexp.MustCompile(`([0-9]*\.?[0-9]+)d`)

func ParseDuration(input string) (time.Duration, error) {
	if strings.HasPrefix(input, "-") {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", input)
	}
	var convErr error
	expanded := dayComponent.ReplaceAllStringFunc(input, func(match string) string {
		days, err := strconv.ParseFloat(strings.TrimSuffix(match, "d"), 64)
		if err != nil {
			convErr = err
			return match
		}
		return strconv.FormatFloat(days*24, 'f', -1, 64) + "h"
	})
	if convErr != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", input, convErr)
	}
	d, err := time.ParseDuration(expanded)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected Go duration syntax with an optional d suffix for days, for example 30d, 12h, or 90m", input)
	}
	return d, nil
}

func Run(ctx context.Context, dbPath string, olderThan time.Duration, now time.Time, out io.Writer) error {
	exists, err := session.DBExists(dbPath)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintln(out, summary(0))
		return nil
	}
	db, err := session.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	deleted, err := deleteInactive(ctx, db, now.Add(-olderThan).UnixMilli())
	if err != nil {
		return err
	}
	for _, publicID := range deleted {
		fmt.Fprintln(out, publicID)
	}
	fmt.Fprintln(out, summary(len(deleted)))
	return nil
}

func deleteInactive(ctx context.Context, db *sql.DB, cutoffMillis int64) ([]string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("prune sessions: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.public_id FROM sessions s
		WHERE COALESCE((SELECT MAX(r.started_at) FROM requests r WHERE r.session_id = s.id), s.created_at) < ?
		ORDER BY s.id`, cutoffMillis)
	if err != nil {
		return nil, fmt.Errorf("find inactive sessions: %w", err)
	}
	var ids []int64
	var publicIDs []string
	for rows.Next() {
		var id int64
		var publicID string
		if err := rows.Scan(&id, &publicID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("find inactive sessions: %w", err)
		}
		ids = append(ids, id)
		publicIDs = append(publicIDs, publicID)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("find inactive sessions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find inactive sessions: %w", err)
	}

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("delete session %s: %w", publicIDs[i], err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM requests WHERE session_id IS NULL AND started_at < ?`, cutoffMillis); err != nil {
		return nil, fmt.Errorf("delete rejected requests: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("prune sessions: %w", err)
	}
	return publicIDs, nil
}

func summary(count int) string {
	if count == 1 {
		return "Deleted 1 session."
	}
	return fmt.Sprintf("Deleted %d sessions.", count)
}
