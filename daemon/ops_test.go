package daemon

import (
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

// TestStatusReportsService checks the "status" op reflects Options.Service.
func TestStatusReportsService(t *testing.T) {
	d := New(Options{Person: "alex", Machine: "mbp", Store: storetest.SQLite(t), Adapters: adapter.NewRegistry(),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: t.Logf, Service: true})

	res := call[StatusResult](t, d, &fakeConn{id: 1}, "status", nil)
	if !res.Service {
		t.Fatalf("status = %+v, want Service=true", res)
	}
	if res.PID == 0 {
		t.Fatalf("status = %+v, want a nonzero PID", res)
	}
}

// TestStatusReportsNonService checks a daemon started without --service reports Service=false.
func TestStatusReportsNonService(t *testing.T) {
	d := newTestDaemon(t)
	res := call[StatusResult](t, d, &fakeConn{id: 1}, "status", nil)
	if res.Service {
		t.Fatalf("status = %+v, want Service=false", res)
	}
}
