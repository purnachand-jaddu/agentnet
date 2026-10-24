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

// `up` must report a pending join request without asking, instead of treating it as "not set up".
func TestUpShowsPendingRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	withIO(t)
	if code := Main([]string{"join", "--person", "sam", "--machine", "xps"}, "v0"); code != 0 {
		t.Fatal("join failed")
	}

	withApprover(t, denyApprover{}) // up must not need to ask when a request is already pending
	out, _ := withIO(t)
	if code := Main([]string{"up"}, "v0"); code != 0 {
		t.Fatalf("up: code %d", code)
	}
	if !strings.Contains(out.String(), "waiting for the founder to admit you") {
		t.Fatalf("up output = %q", out.String())
	}
	if pairingCodeRE.FindString(out.String()) == "" {
		t.Fatalf("up output missing a pairing code: %q", out.String())
	}
}
