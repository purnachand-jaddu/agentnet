//go:build e2e

package cli

import (
	"context"
	"os"
	"runtime"

	"github.com/purnachand-jaddu/agentnet/consent"
)

// init swaps in an auto-approving approver for AGENTNET_TEST_APPROVE=yes; any other env behaves
// like a release build. This file only exists in -tags e2e builds, so a release binary never has it.
func init() {
	newApprover = func() consent.Approver {
		if os.Getenv("AGENTNET_TEST_APPROVE") == "yes" {
			return testApprover{}
		}
		return consent.Default(runtime.GOOS, os.Getenv, os.Stdin, os.Stderr)
	}
}

// testApprover approves every confirmation and picks the first option offered.
type testApprover struct{}

func (testApprover) Confirm(ctx context.Context, title, detail string) (bool, error) {
	return true, nil
}

func (testApprover) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	if len(options) == 0 {
		return "", nil
	}
	return options[0], nil
}
