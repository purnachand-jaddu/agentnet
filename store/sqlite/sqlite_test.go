package sqlite_test

import (
	"os"
	"path/filepath"
	"testing"

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
