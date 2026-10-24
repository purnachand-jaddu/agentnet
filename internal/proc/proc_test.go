package proc

import (
	"errors"
	"os"
	"testing"
)

func TestAgentPID(t *testing.T) {
	chain := map[int]struct {
		ppid int
		comm string
	}{
		10: {0, "myagent"},
		11: {10, "bash"},
		12: {11, "-zsh"},
		13: {12, "sh"},
		14: {1, "sh"}, // reaches pid 1 without ever finding a non-shell
	}
	fakePS := func(pid int) (int, string, error) {
		p, ok := chain[pid]
		if !ok {
			return 0, "", errors.New("no such process")
		}
		return p.ppid, p.comm, nil
	}

	cases := []struct {
		name  string
		start int
		want  int
	}{
		{"direct agent, not a shell", 10, 10},
		{"one shell in between", 11, 10},
		{"two shells in between", 12, 10},
		{"ps error on the first call falls back to start", 99, 99},
		{"chain reaching pid 1 without a non-shell falls back to start", 14, 14},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AgentPID(c.start, fakePS); got != c.want {
				t.Errorf("AgentPID(%d, ...) = %d, want %d", c.start, got, c.want)
			}
		})
	}
}

func TestAgentPIDStopsAfterEightHops(t *testing.T) {
	calls := 0
	ps := func(pid int) (int, string, error) {
		calls++
		return pid + 1, "bash", nil // always a shell, ppid always "higher" so it never reaches <=1
	}
	if got := AgentPID(1000, ps); got != 1000 {
		t.Errorf("AgentPID = %d, want fallback to start 1000", got)
	}
	if calls > 8 {
		t.Errorf("called ps %d times, want at most 8", calls)
	}
}

func TestPS(t *testing.T) {
	ppid, comm, err := PS(os.Getpid())
	if err != nil {
		t.Fatalf("PS(self) error: %v", err)
	}
	if ppid != os.Getppid() {
		t.Errorf("PS(self) ppid = %d, want %d", ppid, os.Getppid())
	}
	if comm == "" {
		t.Error("PS(self) comm is empty")
	}
}
