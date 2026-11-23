package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zachahn/editor/internal/session"
)

var allowedName = regexp.MustCompile(`\A(ORIGINAL\.txt|CURRENT\.txt|SCRATCH-[0-9]+\.txt)\z`)

var errMissingFile = errors.New("file does not exist")

type Overlay struct {
	dir     string
	mu      sync.Mutex
	changed map[string]string
}

func NewOverlay(dir string) *Overlay {
	return &Overlay{dir: dir, changed: map[string]string{}}
}

func checkName(name string) error {
	if !allowedName.MatchString(name) {
		return fmt.Errorf("file name %q is not allowed; allowed names are ORIGINAL.txt, CURRENT.txt, and SCRATCH-<n>.txt", name)
	}
	return nil
}

func checkWritable(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if name == session.OriginalFile {
		return fmt.Errorf("%s is read-only", session.OriginalFile)
	}
	return nil
}

func (o *Overlay) Read(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	o.mu.Lock()
	content, ok := o.changed[name]
	o.mu.Unlock()
	if ok {
		return content, nil
	}
	return o.readDisk(name)
}

func (o *Overlay) readDisk(name string) (string, error) {
	path := filepath.Join(o.dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", name, errMissingFile)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(data), nil
}

func (o *Overlay) Write(name, content string) error {
	if err := checkWritable(name); err != nil {
		return err
	}
	o.mu.Lock()
	o.changed[name] = content
	o.mu.Unlock()
	return nil
}

func (o *Overlay) Names() ([]string, error) {
	set := map[string]bool{}
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, fmt.Errorf("list session: %w", err)
	}
	for _, entry := range entries {
		if allowedName.MatchString(entry.Name()) && entry.Type().IsRegular() {
			set[entry.Name()] = true
		}
	}
	o.mu.Lock()
	for name := range o.changed {
		set[name] = true
	}
	o.mu.Unlock()

	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return fileOrder(names[i]) < fileOrder(names[j]) })
	return names, nil
}

func fileOrder(name string) string {
	switch name {
	case session.OriginalFile:
		return "0"
	case session.CurrentFile:
		return "1"
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "SCRATCH-"), ".txt")
	return fmt.Sprintf("2%020s", digits)
}

func (o *Overlay) Commit() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	var scratch []string
	for name := range o.changed {
		if name != session.CurrentFile {
			scratch = append(scratch, name)
		}
	}
	sort.Slice(scratch, func(i, j int) bool { return fileOrder(scratch[i]) < fileOrder(scratch[j]) })

	for _, name := range scratch {
		if err := session.WriteFileAtomic(o.dir, name, o.changed[name]); err != nil {
			return err
		}
	}
	if content, ok := o.changed[session.CurrentFile]; ok {
		if err := session.WriteFileAtomic(o.dir, session.CurrentFile, content); err != nil {
			return err
		}
	}
	return nil
}
