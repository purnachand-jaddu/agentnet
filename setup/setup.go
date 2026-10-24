// Package setup wires Claude Code and Codex to agentnet and installs the background service.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Runner executes an external command, returning its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec is the real Runner: it shells out, bounded by a 10s timeout.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Step is one idempotent unit of setup.
type Step struct {
	Name   string
	Apply  func() error
	Remove func() error
	Check  func() (bool, error)
}

// Result is one step's outcome from Run.
type Result struct {
	Name string
	Err  error
}

// Run applies (or, if remove, removes) every step, continuing past failures and reporting each step's result.
func Run(steps []Step, remove bool) []Result {
	results := make([]Result, len(steps))
	for i, s := range steps {
		fn := s.Apply
		if remove {
			fn = s.Remove
		}
		results[i] = Result{Name: s.Name, Err: fn()}
	}
	return results
}

// goos is runtime.GOOS; a var so tests can force either OS's service step.
var goos = runtime.GOOS

// Plan builds the setup steps for this machine: Claude Code and Codex when present, and the background service.
func Plan(home, exe string, run Runner) []Step {
	var steps []Step
	var binDirs []string

	if p, err := exec.LookPath("claude"); err == nil {
		binDirs = append(binDirs, filepath.Dir(p))
		steps = append(steps, claudeStep(home, exe, run))
	}

	codexPath, codexErr := exec.LookPath("codex")
	_, codexDirErr := os.Stat(filepath.Join(home, ".codex"))
	if codexErr == nil {
		binDirs = append(binDirs, filepath.Dir(codexPath))
	}
	if codexErr == nil || codexDirErr == nil {
		steps = append(steps, codexStep(home, exe, run))
	}

	steps = append(steps, serviceStep(home, exe, run, goos, binDirs))
	return steps
}

// hookEventOrder is the five lifecycle events setup wires hooks for.
var hookEventOrder = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"}

// hookKebab maps each event to the kebab-case name agentnet's hook command takes.
var hookKebab = map[string]string{
	"SessionStart":     "session-start",
	"UserPromptSubmit": "user-prompt-submit",
	"PostToolUse":      "post-tool-use",
	"Stop":             "stop",
	"SessionEnd":       "session-end",
}

// applyHooks merges our five hook groups into path's "hooks" object, preserving everything else.
func applyHooks(path, exe, agent string) error {
	if err := backupOnce(path); err != nil {
		return err
	}
	doc, err := loadJSON(path)
	if err != nil {
		return err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, event := range hookEventOrder {
		cmd := fmt.Sprintf("%s hook %s %s", shellQuoteIfNeeded(exe), agent, hookKebab[event])
		groups, _ := hooks[event].([]any)
		hooks[event] = upsertHookGroup(groups, exe, agent, cmd)
	}
	doc["hooks"] = hooks
	return saveJSON(path, doc)
}

// hooksWired reports whether every one of our five lifecycle hooks is present in path's "hooks"
// object for agent's commands built from exe, read-only (a missing file is simply not wired).
func hooksWired(path, exe, agent string) (bool, error) {
	doc, err := loadJSON(path)
	if err != nil {
		return false, err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	for _, event := range hookEventOrder {
		groups, _ := hooks[event].([]any)
		found := false
		for _, g := range groups {
			if groupIsOurs(g, exe, agent) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// removeHooks deletes only our hook entries from path's "hooks" object, dropping empty arrays and objects.
func removeHooks(path, exe, agent string) error {
	if err := backupOnce(path); err != nil {
		return err
	}
	doc, err := loadJSON(path)
	if err != nil {
		return err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	for _, event := range hookEventOrder {
		groups, _ := hooks[event].([]any)
		groups = removeOurGroups(groups, exe, agent)
		if len(groups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = groups
		}
	}
	if len(hooks) == 0 {
		delete(doc, "hooks")
	} else {
		doc["hooks"] = hooks
	}
	return saveJSON(path, doc)
}

// newHookGroup is one Claude-Code/Codex hook group running cmd on every match.
func newHookGroup(cmd string) map[string]any {
	return map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	}
}

// groupCommand returns a hook group's command, or "" if it isn't shaped the way newHookGroup makes it.
func groupCommand(group any) string {
	g, ok := group.(map[string]any)
	if !ok {
		return ""
	}
	hooks, ok := g["hooks"].([]any)
	if !ok || len(hooks) != 1 {
		return ""
	}
	h, ok := hooks[0].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := h["command"].(string)
	return cmd
}

// isOurHook reports whether cmd is one of agentnet's own hook commands for agent, matching both
// the quoted and unquoted forms of exe (so an older, pre-quoting hook is still found on remove).
func isOurHook(cmd, exe, agent string) bool {
	if strings.HasPrefix(cmd, shellQuoteIfNeeded(exe)+" hook ") || strings.HasPrefix(cmd, exe+" hook ") {
		return true
	}
	marker := " hook " + agent + " "
	return strings.Contains(cmd, marker) && strings.Contains(cmd, filepath.Base(exe))
}

func groupIsOurs(group any, exe, agent string) bool {
	return isOurHook(groupCommand(group), exe, agent)
}

// upsertHookGroup replaces our existing group in place, or appends one if we don't have one yet.
func upsertHookGroup(groups []any, exe, agent, cmd string) []any {
	for i, g := range groups {
		if groupIsOurs(g, exe, agent) {
			groups[i] = newHookGroup(cmd)
			return groups
		}
	}
	return append(groups, newHookGroup(cmd))
}

// removeOurGroups returns groups without any of our own entries.
func removeOurGroups(groups []any, exe, agent string) []any {
	out := groups[:0]
	for _, g := range groups {
		if !groupIsOurs(g, exe, agent) {
			out = append(out, g)
		}
	}
	return out
}

// isUnquotedSafe reports whether r needs no quoting in a shell word or a systemd Exec= token.
func isUnquotedSafe(r rune) bool {
	return r == '_' || r == '.' || r == '/' || r == '-' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// shellQuoteIfNeeded single-quotes s for a POSIX shell if it contains anything outside [A-Za-z0-9_./-].
func shellQuoteIfNeeded(s string) string {
	for _, r := range s {
		if !isUnquotedSafe(r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// backupOnce copies path to path+".agentnet-bak" the first time it's touched; a no-op once that
// backup exists, or if path doesn't exist yet.
func backupOnce(path string) error {
	bak := path + ".agentnet-bak"
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(bak, data, 0o600)
}

// loadJSON reads path as a JSON object, treating a missing file as {}. Numbers decode as
// json.Number so re-encoding doesn't reformat them.
func loadJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// saveJSON writes doc to path as indented JSON with HTML-escaping off, atomically (temp file +
// rename, keeping path's mode), resolving path's own symlink first so a dotfiles link survives.
func saveJSON(path string, doc map[string]any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil { // Encode appends the trailing newline
		return err
	}

	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode()
	}
	tmp, err := os.CreateTemp(dir, ".setup-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, target)
}
