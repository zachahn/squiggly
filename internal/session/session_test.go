package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRootUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, _ := Root(); got != "/state/editor/sessions" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, _ := Root(); got != "/home/u/.local/state/editor/sessions" {
		t.Fatalf("got %q", got)
	}
}

func TestCreateAndFind(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))
	store.Now = func() time.Time { return time.Unix(1234, 0) }

	sess, err := store.Create("orig", "sys")
	if err != nil {
		t.Fatal(err)
	}
	if !IDPattern.MatchString(sess.ID) || filepath.Base(sess.Dir) != sess.ID+"-1234" {
		t.Fatalf("session = %+v", sess)
	}
	info, _ := os.Stat(sess.Dir)
	if info.Mode().Perm() != DirMode {
		t.Errorf("dir mode = %v", info.Mode().Perm())
	}
	for name, want := range map[string]string{OriginalFile: "orig", CurrentFile: "orig", SystemFile: "sys"} {
		got, err := sess.ReadFile(name)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
		info, _ := os.Stat(filepath.Join(sess.Dir, name))
		if info.Mode().Perm() != FileMode {
			t.Errorf("%s mode = %v", name, info.Mode().Perm())
		}
	}

	found, err := store.Find(sess.ID)
	if err != nil || found.Dir != sess.Dir {
		t.Fatalf("Find = %+v, %v", found, err)
	}
	if _, err := store.Find("00000000"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("unknown err = %v", err)
	}
	if _, err := store.Find("../x/abc"); err == nil || errors.Is(err, ErrUnknownSession) {
		t.Errorf("invalid id err = %v", err)
	}
}

func TestLockSerializesSameSession(t *testing.T) {
	store := NewStore(t.TempDir())
	unlock := store.Lock("abcdef01")
	acquired := make(chan struct{})
	go func() {
		store.Lock("abcdef01")()
		close(acquired)
	}()
	otherUnlock := store.Lock("12345678")
	otherUnlock()
	select {
	case <-acquired:
		t.Fatal("second lock acquired while first was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-acquired
}
