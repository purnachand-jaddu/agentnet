package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
)

// cmdFolder runs `agentnet folder add|remove|list [path]`.
func cmdFolder(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: agentnet folder add|remove|list [path]")
		return 2
	}
	switch args[0] {
	case "list":
		return folderList()
	case "add":
		return folderAddRemove(args[1:], true)
	case "remove":
		return folderAddRemove(args[1:], false)
	default:
		fmt.Fprintln(stderr, "usage: agentnet folder add|remove|list [path]")
		return 2
	}
}

// folderList prints every opt-in folder, one per line.
func folderList() int {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, f := range cfg.Folders {
		fmt.Fprintln(stdout, f)
	}
	return 0
}

// folderAddRemove implements `folder add`/`folder remove` (path defaults to cwd), both gated on consent.
func folderAddRemove(args []string, add bool) int {
	path := "."
	switch len(args) {
	case 0:
	case 1:
		path = args[0]
	default:
		fmt.Fprintln(stderr, "usage: agentnet folder add|remove [path]")
		return 1
	}
	abs, err := resolvePath(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	verb := "Allow"
	if !add {
		verb = "Remove"
	}
	ok, err := newApprover().Confirm(context.Background(), verb+" agentnet for this folder?", abs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	if err := paths.Ensure(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfgPath := paths.Config()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if add {
		cfg.Folders = appendUnique(cfg.Folders, abs)
	} else {
		cfg.Folders = removeString(cfg.Folders, abs)
	}
	if err := cfg.Save(cfgPath); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, abs)
	return 0
}

// appendUnique appends s to list unless it's already there.
func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// removeString returns list without s.
func removeString(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// resolvePath makes p absolute and resolves symlinks, falling back to the absolute form if p doesn't exist yet.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	return abs, nil
}
