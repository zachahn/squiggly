package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	OriginalFile = "ORIGINAL.txt"
	CurrentFile  = "CURRENT.txt"
	SystemFile   = "SYSTEM.txt"

	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

var (
	IDPattern      = regexp.MustCompile(`\A[0-9a-f]{8}\z`)
	DirNamePattern = regexp.MustCompile(`\A([0-9a-f]{8})-([0-9]+)\z`)

	ErrUnknownSession = errors.New("unknown session")
)

func Root() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "editor", "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "editor", "sessions"), nil
}

type Store struct {
	Root string
	Now  func() time.Time

	createMu sync.Mutex
	locksMu  sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewStore(root string) *Store {
	return &Store{Root: root, Now: time.Now, locks: map[string]*sync.Mutex{}}
}

type Session struct {
	ID  string
	Dir string
}

func (s *Store) Lock(id string) func() {
	s.locksMu.Lock()
	mu, ok := s.locks[id]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[id] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Store) Create(original, systemPrompt string) (*Session, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()

	if err := os.MkdirAll(s.Root, DirMode); err != nil {
		return nil, fmt.Errorf("create sessions directory: %w", err)
	}

	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, id+"-"+strconv.FormatInt(s.Now().Unix(), 10))
	if err := os.Mkdir(dir, DirMode); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("set session directory mode: %w", err)
	}

	files := []struct{ name, content string }{
		{OriginalFile, original},
		{CurrentFile, original},
		{SystemFile, systemPrompt},
	}
	for _, f := range files {
		if err := WriteFileAtomic(dir, f.name, f.content); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	return &Session{ID: id, Dir: dir}, nil
}

func (s *Store) newID() (string, error) {
	for {
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate session id: %w", err)
		}
		id := hex.EncodeToString(buf)
		matches, err := filepath.Glob(filepath.Join(s.Root, id+"-*"))
		if err != nil {
			return "", fmt.Errorf("check session id: %w", err)
		}
		if len(matches) == 0 {
			return id, nil
		}
	}
}

func (s *Store) Find(id string) (*Session, error) {
	if !IDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid session_id %q: must be 8 lowercase hexadecimal characters", id)
	}
	matches, err := filepath.Glob(filepath.Join(s.Root, id+"-*"))
	if err != nil {
		return nil, fmt.Errorf("look up session: %w", err)
	}
	for _, match := range matches {
		info, err := os.Stat(match)
		if err == nil && info.IsDir() {
			return &Session{ID: id, Dir: match}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownSession, id)
}

func (s *Session) ReadFile(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, name))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(data), nil
}

func (s *Session) Delete() error {
	return os.RemoveAll(s.Dir)
}

func WriteFileAtomic(dir, name, content string) error {
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, FileMode)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Chmod(FileMode); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}
