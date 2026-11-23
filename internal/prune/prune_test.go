package prune

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":   30 * 24 * time.Hour,
		"12h":   12 * time.Hour,
		"90m":   90 * time.Minute,
		"1d12h": 36 * time.Hour,
		"1.5d":  36 * time.Hour,
	}
	for input, want := range cases {
		got, err := ParseDuration(input)
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "d", "abc", "-1d", "5x"} {
		if _, err := ParseDuration(input); err == nil {
			t.Errorf("%q: expected error", input)
		}
	}
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1_000_000, 0)
	for _, name := range []string{
		"aaaaaaaa-" + "900000",
		"bbbbbbbb-" + "999999",
		"cccccccc-" + "100",
		"notasession-100",
		"ABCDEF12-100",
	} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Run(root, time.Hour, now, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "aaaaaaaa\ncccccccc\nDeleted 2 sessions.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	for name, exists := range map[string]bool{"aaaaaaaa-900000": false, "bbbbbbbb-999999": true, "cccccccc-100": false, "notasession-100": true, "ABCDEF12-100": true} {
		_, err := os.Stat(filepath.Join(root, name))
		if (err == nil) != exists {
			t.Errorf("%s exists = %v, want %v", name, err == nil, exists)
		}
	}
}

func TestRunMissingDirectory(t *testing.T) {
	var out bytes.Buffer
	if err := Run(filepath.Join(t.TempDir(), "missing"), time.Hour, time.Now(), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Deleted 0 sessions.\n" {
		t.Fatalf("output = %q", out.String())
	}
}
