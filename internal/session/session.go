package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	OriginalFile = "ORIGINAL.txt"
	CurrentFile  = "CURRENT.txt"

	ToolStartRewrite  = "start_rewrite"
	ToolReviseRewrite = "revise_rewrite"

	StatusRunning = "running"
	StatusOK      = "ok"
	StatusError   = "error"
)

var (
	IDPattern      = regexp.MustCompile(`\A[0-9a-f]{8}\z`)
	scratchPattern = regexp.MustCompile(`\ASCRATCH-[0-9]+\.txt\z`)

	ErrUnknownSession  = errors.New("unknown session")
	ErrVersionConflict = errors.New("the session was revised by another process during the run")
)

func IsScratchName(name string) bool {
	return scratchPattern.MatchString(name)
}

type RequestSettings struct {
	Platform              string
	Model                 string
	MaxSteps              int
	RequestTimeoutSeconds int
}

func (r RequestSettings) runBudget() time.Duration {
	return time.Duration(r.MaxSteps) * time.Duration(r.RequestTimeoutSeconds) * time.Second
}

type Store struct {
	DB       *sql.DB
	Settings RequestSettings
	Now      func() time.Time
	NewID    func() (string, error)

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewStore(db *sql.DB, settings RequestSettings) *Store {
	return &Store{DB: db, Settings: settings, Now: time.Now, NewID: RandomID, locks: map[string]*sync.Mutex{}}
}

func RandomID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (s *Store) Lock(publicID string) func() {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	mu, ok := s.locks[publicID]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[publicID] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Store) nowMillis() int64 {
	return s.Now().UnixMilli()
}

type Session struct {
	ID           int64
	PublicID     string
	SystemPrompt string
	Instructions string
}

type NewSession struct {
	Text         string
	Instructions string
	SystemPrompt string
	Arguments    string
}

type Request struct {
	ID       int64
	Deadline time.Time
}

func (s *Store) Create(ctx context.Context, params NewSession) (*Session, *Request, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create session: %w", err)
	}
	defer tx.Rollback()

	now := s.nowMillis()
	sess := &Session{SystemPrompt: params.SystemPrompt, Instructions: params.Instructions}
	for {
		publicID, err := s.NewID()
		if err != nil {
			return nil, nil, err
		}
		result, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (public_id, system_prompt, instructions, version, created_at) VALUES (?, ?, ?, 0, ?)`,
			publicID, params.SystemPrompt, params.Instructions, now)
		if isUniqueViolation(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("create session: %w", err)
		}
		sess.PublicID = publicID
		sess.ID, err = result.LastInsertId()
		if err != nil {
			return nil, nil, fmt.Errorf("create session: %w", err)
		}
		break
	}

	for _, file := range []struct {
		name     string
		writable bool
	}{{OriginalFile, false}, {CurrentFile, true}} {
		fileID, err := insertFile(ctx, tx, sess.ID, file.name, file.writable, now)
		if err != nil {
			return nil, nil, fmt.Errorf("create session: %w", err)
		}
		if err := insertVersion(ctx, tx, fileID, nil, params.Text, now); err != nil {
			return nil, nil, fmt.Errorf("create session: %w", err)
		}
	}

	req, err := s.insertRunningRequest(ctx, tx, sess.ID, ToolStartRewrite, params.Arguments)
	if err != nil {
		return nil, nil, fmt.Errorf("create session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("create session: %w", err)
	}
	return sess, req, nil
}

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func (s *Store) Find(ctx context.Context, publicID string) (*Session, error) {
	sess := &Session{PublicID: publicID}
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, system_prompt, instructions FROM sessions WHERE public_id = ?`, publicID,
	).Scan(&sess.ID, &sess.SystemPrompt, &sess.Instructions)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, publicID)
	}
	if err != nil {
		return nil, fmt.Errorf("look up session %s: %w", publicID, err)
	}
	return sess, nil
}

func (s *Store) StartRequest(ctx context.Context, sessionID int64, tool, arguments string) (*Request, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("record request: %w", err)
	}
	defer tx.Rollback()
	req, err := s.insertRunningRequest(ctx, tx, sessionID, tool, arguments)
	if err != nil {
		return nil, fmt.Errorf("record request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("record request: %w", err)
	}
	return req, nil
}

func (s *Store) insertRunningRequest(ctx context.Context, tx *sql.Tx, sessionID int64, tool, arguments string) (*Request, error) {
	started := s.Now()
	deadline := started.Add(s.Settings.runBudget())
	result, err := tx.ExecContext(ctx,
		`INSERT INTO requests (session_id, tool, arguments, status, platform, model, started_at, deadline_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, tool, arguments, StatusRunning, s.Settings.Platform, s.Settings.Model,
		started.UnixMilli(), deadline.UnixMilli())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Request{ID: id, Deadline: time.UnixMilli(deadline.UnixMilli())}, nil
}

func (s *Store) RecordRejected(ctx context.Context, tool, arguments, message string) error {
	now := s.nowMillis()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO requests (session_id, tool, arguments, status, error, platform, model, started_at, deadline_at, finished_at)
		 VALUES (NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tool, arguments, StatusError, message, s.Settings.Platform, s.Settings.Model, now, now, now)
	if err != nil {
		return fmt.Errorf("record rejected request: %w", err)
	}
	return nil
}

type BaseFile struct {
	Writable   bool
	Content    string
	HasVersion bool
}

type Snapshot struct {
	Version int64
	Files   map[string]BaseFile
}

func (s *Store) Snapshot(ctx context.Context, sessionID int64) (*Snapshot, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	defer tx.Rollback()

	snap := &Snapshot{Files: map[string]BaseFile{}}
	if err := tx.QueryRowContext(ctx, `SELECT version FROM sessions WHERE id = ?`, sessionID).Scan(&snap.Version); err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT f.name, f.writable,
		       (SELECT v.content FROM file_versions v
		        LEFT JOIN requests r ON r.id = v.request_id
		        WHERE v.file_id = f.id AND (v.request_id IS NULL OR r.status = ?)
		        ORDER BY v.id DESC LIMIT 1)
		FROM files f WHERE f.session_id = ?`, StatusOK, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read session files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var writable bool
		var content sql.NullString
		if err := rows.Scan(&name, &writable, &content); err != nil {
			return nil, fmt.Errorf("read session files: %w", err)
		}
		snap.Files[name] = BaseFile{Writable: writable, Content: content.String, HasVersion: content.Valid}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read session files: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	return snap, nil
}

func (s *Store) Commit(ctx context.Context, sessionID, requestID, baseVersion int64, changed map[string]string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("commit run: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx,
		`UPDATE sessions SET version = version + 1 WHERE id = ? AND version = ?`, sessionID, baseVersion)
	if err != nil {
		return fmt.Errorf("commit run: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("commit run: %w", err)
	}
	if updated == 0 {
		return ErrVersionConflict
	}

	now := s.nowMillis()
	if err := writeVersions(ctx, tx, sessionID, requestID, changed, now); err != nil {
		return fmt.Errorf("commit run: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE requests SET status = ?, finished_at = ? WHERE id = ?`, StatusOK, now, requestID); err != nil {
		return fmt.Errorf("commit run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run: %w", err)
	}
	return nil
}

func (s *Store) Fail(ctx context.Context, sessionID, requestID int64, changed map[string]string, message string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	defer tx.Rollback()

	now := s.nowMillis()
	if err := writeVersions(ctx, tx, sessionID, requestID, changed, now); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE requests SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		StatusError, message, now, requestID); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	return nil
}

func writeVersions(ctx context.Context, tx *sql.Tx, sessionID, requestID int64, changed map[string]string, now int64) error {
	names := make([]string, 0, len(changed))
	for name := range changed {
		names = append(names, name)
	}
	SortFileNames(names)
	for _, name := range names {
		fileID, err := insertFile(ctx, tx, sessionID, name, true, now)
		if err != nil {
			return err
		}
		if err := insertVersion(ctx, tx, fileID, &requestID, changed[name], now); err != nil {
			return err
		}
	}
	return nil
}

func insertFile(ctx context.Context, tx *sql.Tx, sessionID int64, name string, writable bool, now int64) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files (session_id, name, writable, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (session_id, name) DO NOTHING`,
		sessionID, name, writable, now); err != nil {
		return 0, fmt.Errorf("insert file %s: %w", name, err)
	}
	var fileID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM files WHERE session_id = ? AND name = ?`, sessionID, name).Scan(&fileID); err != nil {
		return 0, fmt.Errorf("insert file %s: %w", name, err)
	}
	return fileID, nil
}

func insertVersion(ctx context.Context, tx *sql.Tx, fileID int64, requestID *int64, content string, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO file_versions (file_id, request_id, content, created_at) VALUES (?, ?, ?, ?)`,
		fileID, requestID, content, now); err != nil {
		return fmt.Errorf("insert file version: %w", err)
	}
	return nil
}

type Message struct {
	Role             string
	Content          string
	Reasoning        string
	ToolCalls        *string
	ToolCallID       *string
	ToolName         *string
	PromptTokens     *int64
	CompletionTokens *int64
	TotalTokens      *int64
}

func (s *Store) AppendMessage(ctx context.Context, requestID int64, msg Message) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO messages (request_id, role, content, reasoning, tool_calls, tool_call_id, tool_name,
		                       prompt_tokens, completion_tokens, total_tokens, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		requestID, msg.Role, msg.Content, msg.Reasoning, msg.ToolCalls, msg.ToolCallID, msg.ToolName,
		msg.PromptTokens, msg.CompletionTokens, msg.TotalTokens, s.nowMillis())
	if err != nil {
		return fmt.Errorf("record message: %w", err)
	}
	return nil
}

func SortFileNames(names []string) {
	sort.Slice(names, func(i, j int) bool { return fileOrder(names[i]) < fileOrder(names[j]) })
}

func fileOrder(name string) string {
	switch name {
	case OriginalFile:
		return "0"
	case CurrentFile:
		return "1"
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "SCRATCH-"), ".txt")
	return fmt.Sprintf("2%020s", digits)
}
