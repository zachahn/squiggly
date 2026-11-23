package prune

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

func Run(root string, olderThan time.Duration, now time.Time, out io.Writer) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(out, summary(0))
		return nil
	}
	if err != nil {
		return fmt.Errorf("read sessions directory: %w", err)
	}

	deleted := 0
	for _, entry := range entries {
		match := session.DirNamePattern.FindStringSubmatch(entry.Name())
		if match == nil || !entry.IsDir() {
			continue
		}
		created, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			continue
		}
		if now.Sub(time.Unix(created, 0)) <= olderThan {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("delete session %s: %w", match[1], err)
		}
		fmt.Fprintln(out, match[1])
		deleted++
	}
	fmt.Fprintln(out, summary(deleted))
	return nil
}

func summary(count int) string {
	if count == 1 {
		return "Deleted 1 session."
	}
	return fmt.Sprintf("Deleted %d sessions.", count)
}
