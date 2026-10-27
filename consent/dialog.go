package consent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ncruces/zenity"
)

// Dialog asks a person through a system dialog, using the pure-Go zenity driver
// (github.com/ncruces/zenity): native dialogs on Windows, osascript on macOS, and
// zenity, qarma or matedialog on Linux. Pick is the backend call; tests stub it,
// Default wires the real zenity one.
type Dialog struct {
	// Pick shows a single-choice list and returns zenity.ErrCanceled when the person cancels.
	Pick func(ctx context.Context, title, detail string, options []string) (string, error)
}

// pickZenity shows a single-choice list through zenity.
func pickZenity(ctx context.Context, title, detail string, options []string) (string, error) {
	return zenity.List(detail, options, zenity.Title(title), zenity.Context(ctx))
}

// confirmChoice is the option a person must pick to approve. Denial is every other
// outcome — the picker's raw result is never trusted.
const approveChoice = "Approve"

// confirmGiveUp is how long Confirm waits before silently denying, like the old
// "giving up after 300" dialog: callers do not set deadlines of their own.
const confirmGiveUp = 5 * time.Minute

// Confirm approves only when the person explicitly picked Approve from the list.
// An untouched list answers Deny or empty —cancel, window close and giving up read
// as denials too. It gives up and denies after five minutes.
func (d Dialog) Confirm(ctx context.Context, title, detail string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, confirmGiveUp)
	defer cancel()
	choice, err := d.pick(ctx, title, detail, "Deny", approveChoice)
	if err != nil {
		return false, err
	}
	return choice == approveChoice, nil
}

// Choose returns the chosen option, or "" on cancel, gave up or a bogus result.
// A result not among the offered options is never trusted.
func (d Dialog) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	choice, err := d.pick(ctx, title, detail, options...)
	if err != nil {
		return "", err
	}
	for _, o := range options {
		if choice == o {
			return choice, nil
		}
	}
	return "", nil
}

// pick maps backend errors through consent's taxonomy: cancel and gave up are
// silent denials, no GUI session is ErrNoHuman, anything else is reported. The
// backend maps every exit status 1 to ErrCanceled, so some backend failures deny
// silently instead of reporting — they never approve.
func (d Dialog) pick(ctx context.Context, title, detail string, options ...string) (string, error) {
	choice, err := d.Pick(ctx, title, detail, options)
	switch {
	case err == nil:
		return choice, nil
	case errors.Is(err, zenity.ErrCanceled), errors.Is(err, context.DeadlineExceeded):
		return "", nil
	case noGUI(err):
		return "", ErrNoHuman
	}
	return "", err
}

// noGUI reports "there is nobody on screen to ask": macOS reports that as
// osascript error -1713 ("No user interaction allowed"). Unrecognized failures
// propagate as errors instead — they deny, never approve.
func noGUI(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no user interaction") || strings.Contains(s, "-1713")
}
