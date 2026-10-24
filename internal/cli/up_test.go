package cli

import (
	"strings"
	"testing"
)

// `up` must report status without asking, once this machine already belongs to a network.
func TestUpPrintsStatusWhenAlreadyInNetwork(t *testing.T) {
	bundle, teamCode, _, joinerHome := bundleAndCodes(t)

	t.Setenv("AGENTNET_HOME", joinerHome)
	withApprover(t, fakeApprover{})
	out, errOut := withIO(t)
	if code := Main([]string{"accept", bundle}, "v0"); code != 0 {
		t.Fatalf("accept setup failed: stdout %q, stderr %q", out.String(), errOut.String())
	}

	withApprover(t, denyApprover{}) // up must not need to ask when already in a network
	out, _ = withIO(t)
	if code := Main([]string{"up"}, "v0"); code != 0 {
		t.Fatalf("up: code %d", code)
	}
	if !strings.Contains(out.String(), "In network "+teamCode) {
		t.Fatalf("up output = %q, want team code %q", out.String(), teamCode)
	}
}
