package storetest

import (
	"path/filepath"
	"testing"

	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
)

// SQLite opens a store on a temp file for tests.
func SQLite(t *testing.T) store.Store {
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
