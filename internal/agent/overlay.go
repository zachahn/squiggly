package agent

import (
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/zachahn/editor/internal/session"
)

var allowedName = regexp.MustCompile(`\A(ORIGINAL\.txt|CURRENT\.txt|SCRATCH-[0-9]+\.txt)\z`)

var errMissingFile = errors.New("file does not exist")

type Overlay struct {
	base    map[string]session.BaseFile
	mu      sync.Mutex
	changed map[string]string
}

func NewOverlay(snapshot *session.Snapshot) *Overlay {
	base := map[string]session.BaseFile{}
	if snapshot != nil {
		for name, file := range snapshot.Files {
			base[name] = file
		}
	}
	return &Overlay{base: base, changed: map[string]string{}}
}

func checkName(name string) error {
	if !allowedName.MatchString(name) {
		return fmt.Errorf("file name %q is not allowed; allowed names are ORIGINAL.txt, CURRENT.txt, and SCRATCH-<n>.txt", name)
	}
	return nil
}

func (o *Overlay) CheckWritable(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if file, ok := o.base[name]; ok {
		if !file.Writable {
			return fmt.Errorf("%s is read-only", name)
		}
		return nil
	}
	if !session.IsScratchName(name) {
		return fmt.Errorf("%s cannot be created; only SCRATCH-<n>.txt files may be created", name)
	}
	return nil
}

func (o *Overlay) Read(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if content, ok := o.changed[name]; ok {
		return content, nil
	}
	if file, ok := o.base[name]; ok && file.HasVersion {
		return file.Content, nil
	}
	return "", fmt.Errorf("%s: %w", name, errMissingFile)
}

func (o *Overlay) Write(name, content string) error {
	if err := o.CheckWritable(name); err != nil {
		return err
	}
	o.mu.Lock()
	o.changed[name] = content
	o.mu.Unlock()
	return nil
}

func (o *Overlay) Names() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := map[string]bool{}
	for name, file := range o.base {
		if file.HasVersion {
			set[name] = true
		}
	}
	for name := range o.changed {
		set[name] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	session.SortFileNames(names)
	return names
}

func (o *Overlay) Changed() map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	changed := make(map[string]string, len(o.changed))
	for name, content := range o.changed {
		changed[name] = content
	}
	return changed
}
