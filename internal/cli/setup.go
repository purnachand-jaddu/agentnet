package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/consent"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/setup"
)

// cmdSetup runs `agentnet setup [--remove] [--dry-run]`.
func cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "undo setup instead of applying it")
	dryRun := fs.Bool("dry-run", false, "show the steps without changing anything")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	approver := newApprover()
	steps := setup.Plan(home, exe, setup.Exec)
	if !*remove {
		steps = append(steps, folderStep(approver)) // nothing to undo on --remove
	}

	var detail strings.Builder
	for _, s := range steps {
		fmt.Fprintln(&detail, s.Name)
	}

	if *dryRun {
		action := "apply"
		if *remove {
			action = "remove"
		}
		fmt.Fprintf(stdout, "Would %s:\n", action)
		for _, s := range steps {
			already, _ := s.Check()
			status := "not yet applied"
			if already {
				status = "already applied"
			}
			fmt.Fprintf(stdout, "  %s (%s)\n", s.Name, status)
		}
		return 0
	}

	title := "Run agentnet setup?"
	if *remove {
		title = "Remove agentnet setup?"
	}
	ok, err := approver.Confirm(context.Background(), title, detail.String())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	code := 0
	for _, r := range setup.Run(steps, *remove) {
		mark := "\u2713"
		if r.Err != nil {
			mark = "\u2717"
			code = 1
		}
		fmt.Fprintf(stdout, "%s %s\n", mark, r.Name)
		if r.Err != nil {
			fmt.Fprintln(stderr, r.Err)
		}
	}
	return code
}

// folderStep offers to opt the current directory into agentnet, gated on consent.Choose.
func folderStep(approver consent.Approver) setup.Step {
	return setup.Step{
		Name: "Opt this folder in to agentnet",
		Apply: func() error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			cfgPath := paths.Config()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cfg.Allowed(cwd) {
				return nil
			}
			choice, err := approver.Choose(context.Background(), "Opt in this folder?", cwd,
				[]string{"Add this folder", "Skip"})
			if err != nil {
				return err
			}
			if choice != "Add this folder" {
				return nil
			}
			if err := paths.Ensure(); err != nil {
				return err
			}
			cfg.Folders = appendUnique(cfg.Folders, cwd)
			return cfg.Save(cfgPath)
		},
		Remove: func() error { return nil }, // folder opt-in isn't undone by --remove
		Check: func() (bool, error) {
			cwd, err := os.Getwd()
			if err != nil {
				return false, err
			}
			cfg, err := config.Load(paths.Config())
			if err != nil {
				return false, err
			}
			return cfg.Allowed(cwd), nil
		},
	}
}
