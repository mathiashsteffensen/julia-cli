package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/memory"
	"julia/internal/session"
	"julia/internal/ui"
)

// options are the command line's flags. The persistent ones choose what
// every command works with; the rest are the root command's, which runs the
// interface or one headless task.
type options struct {
	config, profile, dir, model, reasoning, mode, timeout string

	prompt, resume, memory string
	cont, demo, json       bool
	snapshot               int
	// consolidate is set by memory consolidate, which runs the root
	// command's headless task on the notes.
	consolidate bool
}

// newCommand is the julia command line. configPath is the default
// configuration file.
func newCommand(configPath string) *cobra.Command {
	o := &options{}
	root := &cobra.Command{
		Use:     "julia",
		Short:   "JULIA · a quiet interface for ambitious work",
		Version: version,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q (use --prompt for a headless task)", args[0])
			}
			return nil
		},
		// main prints the error, made safe for the terminal.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.runSession(cmd)
		},
	}
	root.SetVersionTemplate("julia {{.Version}}\n")

	shared := root.PersistentFlags()
	shared.StringVar(&o.config, "config", configPath, "configuration file")
	shared.StringVar(&o.profile, "profile", "", "configuration profile name (default: default)")
	shared.StringVar(&o.dir, "dir", ".", "workspace directory")
	shared.StringVar(&o.model, "model", "", "provider/<model-id>: codex, claude, openai, anthropic, google or the id of one of your providers (bare codex uses your Codex model, bare claude Claude Code's default)")
	shared.StringVar(&o.reasoning, "reasoning", "", "Reasoning effort (Codex model-supported value, Claude Code's low to max, or reasoning_effort for your providers; model support varies)")
	shared.StringVar(&o.mode, "mode", "", "auto, ask, read-only, or yolo (no sandbox, no review: for an unattended container)")
	shared.StringVar(&o.timeout, "timeout", "", "maximum duration per task, e.g. 4h")

	flags := root.Flags()
	flags.StringVarP(&o.prompt, "prompt", "p", "", "run one task without the TUI")
	flags.StringVar(&o.memory, "memory", "", "off, read or write: whether notes kept between sessions are seen and saved (default write; read for a headless --prompt run)")
	flags.StringVar(&o.resume, "resume", "", "resume a saved session ID")
	flags.BoolVar(&o.cont, "continue", false, "resume latest session in this directory")
	flags.BoolVar(&o.demo, "demo", false, "explore the interface offline")
	flags.BoolVar(&o.json, "json", false, "emit JSON events in headless mode")
	flags.IntVar(&o.snapshot, "snapshot", 0, "render an offline demo at this terminal width")

	root.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Check config, provider, reviewer, subagent roles, search key, memory, browser, rg and bash",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := o.open(cmd)
			if err != nil {
				return err
			}
			return doctor(cmd.Context(), l.cfg, l.path, l.roles, l.dir)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "login [codex|claude]",
		Short: "Sign in to Codex or Claude Code through its own CLI",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := o.open(cmd)
			if err != nil {
				return err
			}
			return login(cmd.Context(), l.cfg, args)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "sessions",
		Short: "List saved sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := o.open(cmd)
			if err != nil {
				return err
			}
			list, err := l.store.List()
			if err != nil {
				return err
			}
			for _, s := range list {
				fmt.Printf("%s  %-10s  %s  %s\n", s.ID, safe(s.Provider), s.UpdatedAt.Format("Jan 02 15:04"), safe(s.Title))
			}
			if len(list) == 0 {
				fmt.Println("No saved sessions yet.")
			}
			return nil
		},
	})

	configCommand := &cobra.Command{
		Use:   "config",
		Short: "Create the configuration file",
		Args:  cobra.NoArgs,
		RunE:  showHelp,
	}
	configCommand.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Create the configuration file, or the --profile one, with the defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.checkProfile(cmd); err != nil {
				return err
			}
			path := o.config
			if o.profile != "" {
				path = config.ProfilePath(filepath.Dir(path), o.profile)
			}
			path, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if err := config.Init(path); err != nil {
				return err
			}
			fmt.Println("Created", path)
			return nil
		},
	})
	root.AddCommand(configCommand)

	notes := memoryCommand(func(cmd *cobra.Command) (string, error) {
		l, err := o.open(cmd)
		if err != nil {
			return "", err
		}
		return l.dir, nil
	}, ui.RunMemoryReview)
	consolidate := &cobra.Command{
		Use:   "consolidate",
		Short: "Merge, tighten and prune the notes in a headless pass with the Memory tool alone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Consolidation is a headless task of the agent's: a pass over the
			// notes with the Memory tool and nothing else, so the workspace
			// tools run read-only while notes are written, and no reflection
			// follows it.
			o.consolidate, o.prompt, o.memory, o.mode = true, memory.ConsolidationPrompt, "write", "read-only"
			return o.runSession(cmd)
		},
	}
	consolidate.Flags().BoolVar(&o.json, "json", false, "emit JSON events")
	notes.AddCommand(consolidate)
	root.AddCommand(notes)

	root.AddCommand(mineCommand(o.openMining))
	return root
}

// rejectSingleDashFlags rejects a long flag written with one dash, as the
// flag package took it: pflag would read -prompt=x as -p with the value
// "rompt=x", and run that as a task.
func rejectSingleDashFlags(root *cobra.Command, args []string) error {
	for _, arg := range args {
		if arg == "--" {
			return nil
		}
		name, oneDash := strings.CutPrefix(arg, "-")
		name, _, _ = strings.Cut(name, "=")
		if oneDash && len(name) > 1 && (root.Flags().Lookup(name) != nil || root.PersistentFlags().Lookup(name) != nil) {
			return fmt.Errorf("%s: write long flags with two dashes, as --%s", arg, name)
		}
	}
	return nil
}

// showHelp runs a command that only groups others. With Args set to
// cobra.NoArgs, an unknown subcommand is then an error, not the help.
func showHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

// launch is what a command works with, as its flags select it.
type launch struct {
	// store holds the saved sessions. Only the root command locks one, and
	// it closes the store.
	store   *session.Store
	dir     string        // the workspace, absolute with symlinks resolved
	session *core.Session // the one --resume or --continue chose, locked; else nil
	path    string        // the configuration file, absolute
	profile string        // its profile; "" for a --config file
	cfg     config.Config
	roles   []agents.Role
}

// open resolves the flags every command shares. A session to resume is
// locked and read first: unless --config or --profile says otherwise, it
// names its own configuration.
func (o *options) open(cmd *cobra.Command) (l *launch, err error) {
	flags := cmd.Flags()
	configSet, profileSet := flags.Changed("config"), flags.Changed("profile")
	if err := o.checkProfile(cmd); err != nil {
		return nil, err
	}

	store, err := session.Default()
	if err != nil {
		return nil, err
	}
	// A launch that fails unlocks the session it resumed.
	defer func() {
		if err != nil {
			store.Close()
		}
	}()
	l = &launch{store: store, path: o.config, profile: o.profile}

	l.dir, err = filepath.Abs(o.dir)
	if err != nil {
		return nil, err
	}
	l.dir, err = filepath.EvalSymlinks(l.dir)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(l.dir); err != nil || !info.IsDir() {
		return nil, errors.New("workspace must be a directory")
	}

	// Select and lock the conversation before loading any launch configuration.
	// The post-lock read is the authority for its profile and model.
	if o.snapshot == 0 && (o.resume != "" || o.cont) {
		l.session, err = openResumeSession(store, l.dir, o.resume, o.cont, !o.demo)
		if err != nil {
			return nil, err
		}
		if saved := l.session; !configSet && !profileSet && saved.ConfigPath != "" {
			l.path, l.profile = saved.ConfigPath, saved.ConfigProfile
			if l.profile == "" {
				if _, err := os.Stat(l.path); err != nil {
					return nil, fmt.Errorf("saved configuration %s: %w", l.path, err)
				}
			}
		}
	}

	// A profile lives beside the default configuration file, or beside the
	// resumed session's.
	if l.profile != "" {
		profileDir := filepath.Dir(l.path)
		l.path = config.ProfilePath(profileDir, l.profile)
		l.cfg, err = config.LoadProfile(profileDir, l.profile)
	} else {
		l.cfg, err = config.Load(l.path)
	}
	if err != nil {
		return nil, err
	}
	l.path, err = filepath.Abs(l.path)
	if err != nil {
		return nil, err
	}

	// The "model" entry's reasoning belongs to that provider and model only.
	backend, model := config.SplitModel(o.model)
	if o.model != "" && !l.cfg.ModelSelectable(backend, model) {
		return nil, fmt.Errorf("--model %q is not in enabled_models", o.model)
	}
	if o.model != "" && (backend != l.cfg.Provider || model != l.cfg.Model) {
		l.cfg.Provider, l.cfg.Model, l.cfg.ModelReasoning = backend, model, ""
	}
	reasoningSet := flags.Changed("reasoning")
	if reasoningSet {
		l.cfg.Reasoning = o.reasoning
	}
	launchOverrides{mode: o.mode, timeout: o.timeout}.apply(&l.cfg)
	// The model's own configured reasoning, else the last effort chosen for
	// this provider, replaces the configured default; an explicit flag is
	// itself a choice and becomes the new default.
	preferencesPath, preferencesErr := config.PreferencesPath()
	if !reasoningSet {
		var prefs config.Preferences
		if preferencesErr == nil {
			prefs, _ = config.LoadPreferences(preferencesPath)
		}
		l.cfg.ApplyStartingReasoning(prefs)
	}
	if err = l.cfg.Validate(); err != nil {
		return nil, err
	}
	if reasoningSet && preferencesErr == nil && !o.demo && o.snapshot == 0 {
		_ = config.RememberReasoning(preferencesPath, l.cfg.Provider, l.cfg.Reasoning)
	}

	l.roles, err = loadRoles(l.path, l.cfg)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// checkProfile rejects a --profile that cannot name a profile.
func (o *options) checkProfile(cmd *cobra.Command) error {
	switch {
	case o.profile != "" && cmd.Flags().Changed("config"):
		return errors.New("choose either --config or --profile")
	case o.profile == "shared":
		return errors.New(`--profile "shared" is reserved for shared configuration`)
	case o.profile != "" && !config.ValidProfileName(o.profile):
		return fmt.Errorf("--profile %q is not a profile name, which is %s", o.profile, config.ProfileRule)
	}
	return nil
}
