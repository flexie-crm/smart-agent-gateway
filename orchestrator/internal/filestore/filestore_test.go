package filestore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func store(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

func TestBytesGoInAndComeBackOut(t *testing.T) {
	s := store(t)
	n, err := s.Write(7, "at_abc123", strings.NewReader("hello"), 1024)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 {
		t.Fatalf("size = %d", n)
	}

	f, err := s.Open(7, "at_abc123")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, _ := os.ReadFile(f.Name())
	if string(got) != "hello" {
		t.Fatalf("read back %q", got)
	}
	// Closed before the removal below rather than at the end of the test, and
	// the order is not tidiness: unix unlinks a file that is still open and
	// Windows refuses to, so leaving this to a defer failed the remove here for
	// a reason that has nothing to do with what the removal is being tested for.
	//
	// It is a real difference and not only a test's problem: taking an
	// attachment away while something is streaming it works on a server and
	// does not on a laptop. Nothing does that today (a delete and a download of
	// the same attachment at the same moment), which is why this is a note
	// rather than a change to how files are opened.
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := s.Remove(7, "at_abc123"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := s.Open(7, "at_abc123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected it to be gone, got %v", err)
	}
	// Removing what is already gone is what the caller wanted.
	if err := s.Remove(7, "at_abc123"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// One workspace's files cannot be reached with another's id, because the
// workspace is part of where they live.
func TestOneWorkspaceCannotReadAnothers(t *testing.T) {
	s := store(t)
	if _, err := s.Write(1, "at_shared", strings.NewReader("first"), 1024); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Open(2, "at_shared"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("workspace 2 could see workspace 1's file: %v", err)
	}
}

// The rule the package exists for. None of these can produce a path outside the
// root, and none of them is sanitised into something usable: they are refused.
func TestAnIdThatIsNotAnIdIsRefused(t *testing.T) {
	s := store(t)
	for _, id := range []string{
		"../escape",
		"../../etc/passwd",
		"a/b",
		`a\b`,
		".hidden",
		"with space",
		"with\x00null",
		"",
		strings.Repeat("x", 65),
	} {
		if _, err := s.Write(1, id, strings.NewReader("x"), 1024); err == nil {
			t.Errorf("%q was accepted as a file id", id)
		}
		if _, err := s.Open(1, id); err == nil {
			t.Errorf("%q was accepted for reading", id)
		}
	}

	// And nothing was written anywhere.
	var found []string
	_ = filepath.Walk(s.Root(), func(path string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("a refused id still wrote something: %v", found)
	}
}

// A file over the limit is refused rather than trimmed: a truncated PDF read as
// if it were whole is worse than one that never arrived.
func TestAFileOverTheLimitIsRefusedAndLeavesNothing(t *testing.T) {
	s := store(t)
	if _, err := s.Write(1, "at_big", strings.NewReader(strings.Repeat("x", 100)), 10); err == nil {
		t.Fatal("a file over the limit was accepted")
	}
	if _, err := s.Open(1, "at_big"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the partial file was left behind")
	}
	// Exactly at the limit is fine: the boundary belongs to the caller.
	if n, err := s.Write(1, "at_exact", strings.NewReader(strings.Repeat("x", 10)), 10); err != nil || n != 10 {
		t.Fatalf("a file exactly at the limit was refused: %v", err)
	}
}

// Writing the same id twice is a bug in the caller, not something to paper over
// by overwriting somebody's file.
func TestTheSameIdIsNotWrittenTwice(t *testing.T) {
	s := store(t)
	if _, err := s.Write(1, "at_once", strings.NewReader("first"), 1024); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Write(1, "at_once", strings.NewReader("second"), 1024); err == nil {
		t.Fatal("the second write silently replaced the first")
	}
}

func TestADirectoryThatCannotBeUsedIsRefusedAtStartup(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("an empty directory was accepted")
	}
	// A path that is a FILE cannot become a folder, and that is a deployment
	// mistake worth hearing about at boot rather than at the first upload.
	f := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := New(f); err == nil {
		t.Fatal("a regular file was accepted as the upload directory")
	}
}
