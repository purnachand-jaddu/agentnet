package sqlite_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "a.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestSurvivesReopenAndIsPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	s, err := sqlite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("ns", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("db mode %v", st.Mode().Perm())
	}
	s, err = sqlite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.Get("ns", "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "v" {
		t.Fatal("data lost on reopen")
	}
}

// TestOpenReadOnlyMissingFileFails proves a reader (doctor) gets a clean error for "no db yet",
// rather than a freshly-created empty database.
func TestOpenReadOnlyMissingFileFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.db")
	if _, err := sqlite.OpenReadOnly(p); err == nil {
		t.Fatal("OpenReadOnly on a missing file should fail, not create one")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly should never create the file; stat err = %v", err)
	}
}

// TestOpenReadOnlyReadsButNeverWrites proves OpenReadOnly can read an existing store's data but a
// write through it fails, and the file's mode, mtime and size are unchanged afterward.
func TestOpenReadOnlyReadsButNeverWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	s, err := sqlite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("ns", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil { // a looser mode OpenReadOnly must not "fix"
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	ro, err := sqlite.OpenReadOnly(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ro.Get("ns", "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "v" {
		t.Fatal("read-only open lost existing data")
	}
	if err := ro.Put("ns", "k2", []byte("v2")); err == nil {
		t.Fatal("Put through a read-only connection should fail")
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Fatalf("mode changed: %v -> %v", before.Mode(), after.Mode())
	}
	if before.ModTime() != after.ModTime() {
		t.Fatalf("mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
	if before.Size() != after.Size() {
		t.Fatalf("size changed: %d -> %d", before.Size(), after.Size())
	}
}

// A database from before messages had an "at" column opens, and its held mail starts a fresh retention clock.
func TestOpenMigratesMessagesWithoutAt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (
  id TEXT PRIMARY KEY, mailbox TEXT NOT NULL, msg TEXT NOT NULL,
  held INTEGER NOT NULL DEFAULT 0, read_at INTEGER, seq INTEGER NOT NULL);
INSERT INTO messages (id, mailbox, msg, held, seq) VALUES ('h1', 'x', '{"id":"h1","from":"sam/a","to":"alex/x","body":"hi"}', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := sqlite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if n, err := s.PruneMessages(now.Add(-8*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("PruneMessages = %d, %v; want 0, nil", n, err)
	}
	if n, err := s.HeldFrom("sam"); err != nil || n != 1 {
		t.Fatalf("HeldFrom = %d, %v; want 1, nil", n, err)
	}
}
