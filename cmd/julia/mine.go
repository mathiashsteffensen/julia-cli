package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/miner"
	"julia/internal/skills"
	"julia/internal/tools"
	"julia/internal/ui"
)

const miningGuide = `Turn saved sessions into a reviewed fine-tuning dataset.

REPORT is a report.json file or its containing directory.
Scan is local and read-only. Judge sends candidate transcripts to your selected
model, with no tools. Review saves every decision; 1–5 accepts and ranks, r rejects,
d defers, and e confirms export, which uses the review's export flags. Export
writes training.jsonl, validation.jsonl, and manifest.json into a new private
directory. No command changes source sessions.`

type miningCommand struct {
	config      config.Config
	sessions    string
	workspace   string
	definitions []core.ToolDefinition
	input       io.Reader
	output      io.Writer
	makeJudge   func(context.Context, config.Config) (core.Completer, error)
	review      func(context.Context, ui.MiningReviewConfig) error
}

// scanFlags are the flags of julia mine scan.
type scanFlags struct {
	options       miner.ScanOptions
	all           bool
	since, output string
}

// reportFlags are the flags of the mining commands that take a report.
type reportFlags struct {
	callTimeout time.Duration // judge's
	output      string        // export's, and review's, which exports too
	export      miner.ExportOptions
}

// mineCommand is julia mine. open gives the mining command for the flags a
// subcommand runs with.
func mineCommand(open func(*cobra.Command) (miningCommand, error)) *cobra.Command {
	mine := &cobra.Command{
		Use:   "mine",
		Short: "Turn saved sessions into a reviewed fine-tuning dataset",
		Long:  miningGuide,
		Args:  cobra.NoArgs,
		RunE:  showHelp,
	}

	var scan scanFlags
	scanCommand := &cobra.Command{
		Use:   "scan",
		Short: "Select candidate turns from saved sessions into a new report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := open(cmd)
			if err != nil {
				return err
			}
			return c.scan(cmd.Context(), scan)
		},
	}
	flags := scanCommand.Flags()
	flags.BoolVar(&scan.all, "all", false, "include every workspace (default: current workspace)")
	flags.StringVar(&scan.options.Provider, "source-provider", "", "only sessions recorded with this provider")
	flags.StringVar(&scan.options.Model, "source-model", "", "only sessions recorded with this exact model ID")
	flags.StringVar(&scan.since, "since", "", "only sessions updated on/after YYYY-MM-DD (UTC)")
	flags.IntVar(&scan.options.Limit, "limit", 100, "maximum candidates, balanced across behavior signals")
	flags.IntVar(&scan.options.MaxBytes, "max-bytes", 48*1024, "maximum transcript plus feedback bytes; larger turns are skipped, not truncated")
	flags.StringVar(&scan.output, "output", "", "new directory for report.json and report.md (default: JULIA state directory)")
	flags.StringVar(&scan.options.Focus, "focus", miner.DefaultFocus, "behavior the judge should evaluate")
	mine.AddCommand(scanCommand)

	for _, action := range []struct{ name, short string }{
		{"judge", "Have the selected model assess each open candidate, with no tools"},
		{"review", "Decide on each candidate: 1–5 accepts and ranks, r rejects, d defers, e exports"},
		{"export", "Write the accepted candidates into a new private dataset directory"},
	} {
		var report reportFlags
		command := &cobra.Command{
			Use:   action.name + " REPORT",
			Short: action.short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := open(cmd)
				if err != nil {
					return err
				}
				return c.withReport(cmd.Context(), action.name, args[0], report)
			},
		}
		flags := command.Flags()
		if action.name == "judge" {
			flags.DurationVar(&report.callTimeout, "call-timeout", 2*time.Minute, "deadline per candidate model call")
		} else {
			// Review exports too, after e and y, so it takes the same export flags.
			flags.StringVar(&report.output, "output", "", "new dataset directory (default: dataset beside report.json)")
			flags.IntVar(&report.export.MinRank, "min-rank", 1, "lowest accepted human rank to export, 1–5")
			flags.IntVar(&report.export.ValidationPercent, "validation-percent", 10, "hold out 0–50 percent by whole conversation group")
		}
		mine.AddCommand(command)
	}
	return mine
}

// openMining is the mining command for the flags cmd runs with. Only a scan
// gets the tool definitions.
func (o *options) openMining(cmd *cobra.Command) (miningCommand, error) {
	l, err := o.open(cmd)
	if err != nil {
		return miningCommand{}, err
	}
	command := miningCommand{
		config: l.cfg, sessions: l.store.Directory, workspace: l.dir, input: os.Stdin, output: cmd.OutOrStdout(),
		review: ui.RunMiningReview,
		makeJudge: func(ctx context.Context, cfg config.Config) (core.Completer, error) {
			backend, _, err := newBackend(ctx, cfg, nil)
			if err != nil {
				return nil, err
			}
			completer, ok := backend.(core.Completer)
			if !ok {
				return nil, errors.New("selected provider does not support isolated completion")
			}
			return completer, nil
		},
	}
	if cmd.Name() == "scan" {
		command.definitions, err = miningDefinitions(l.roles, command.output)
	}
	return command, err
}

// miningDefinitions are the tools a scan reads tool calls against. These are
// normalized deployment schemas, not claims about what a historical session
// saw. Private integration tools are not mined.
func miningDefinitions(roles []agents.Role, output io.Writer) ([]core.ToolDefinition, error) {
	var definitions []core.ToolDefinition
	for _, definition := range tools.Definitions(roles, false, false, false, false) {
		if definition.Name != "Slack" {
			definitions = append(definitions, definition)
		}
	}
	definitions = append(definitions, tools.QuestionDefinition(), tools.ReviewDefinition(), tools.MemoryDefinition())
	// The Skill tool as the runtime offers it: the user's skills folder,
	// without the skills only the user may run.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate skills: %w", err)
	}
	catalog, warnings, err := skills.Discover(skills.DefaultDir(home))
	if err != nil {
		return nil, err
	}
	for _, warning := range warnings {
		fmt.Fprintln(output, safe(warning))
	}
	if offered := catalog.Offered(); len(offered.Skills) > 0 {
		definitions = append(definitions, tools.SkillDefinition(offered))
	}
	return definitions, nil
}

func (c miningCommand) scan(ctx context.Context, f scanFlags) error {
	options := f.options
	if options.Limit <= 0 || options.MaxBytes <= 0 || strings.TrimSpace(options.Focus) == "" {
		return errors.New("scan needs positive limits and a nonempty focus; use julia mine scan --help")
	}
	if !f.all {
		options.Workspace = c.workspace
	}
	if f.since != "" {
		var err error
		options.Since, err = time.Parse("2006-01-02", f.since)
		if err != nil {
			return errors.New("--since must be YYYY-MM-DD")
		}
	}
	output := f.output
	if output == "" {
		output = filepath.Join(filepath.Dir(c.sessions), "mining", time.Now().UTC().Format("20060102T150405.000000000Z"))
	}
	output, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("scan output directory already exists; choose a new --output directory")
	}
	// Scan validates the report header, the focus included, before it reads
	// any session.
	report, err := miner.Scan(ctx, c.sessions, options, c.definitions)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return err
	}
	path := filepath.Join(output, "report.json")
	if err := miner.Save(path, report); err != nil {
		// Removes only the empty directory, so a rerun can use the same --output.
		_ = os.Remove(output)
		return err
	}
	if err := writeMiningSummary(path, report); err != nil {
		return err
	}
	fmt.Fprintf(c.output, "Scanned %d sessions and %d turns; selected %d candidates, removed %d duplicates.\nReport: %s\n",
		report.Stats.Sessions, report.Stats.Turns, len(report.Candidates), report.Stats.Duplicates, safe(path))
	fmt.Fprintln(c.output, "Source sessions are unchanged. Inspect the local report before judging; secret detection is heuristic.")
	fmt.Fprintf(c.output, "Next: julia mine judge %q\n", path)
	return nil
}

func (c miningCommand) withReport(ctx context.Context, action, path string, f reportFlags) error {
	if action == "judge" && f.callTimeout <= 0 {
		return errors.New("--call-timeout must be positive")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "report.json")
	}
	// Do not create a lock file beside something that is not a report. The full
	// validation happens once, under the lock.
	if err := miner.CheckFile(path); err != nil {
		return err
	}
	unlock, err := lockMiningReport(path)
	if err != nil {
		return err
	}
	defer unlock()
	report, err := miner.Load(path)
	if err != nil {
		return err
	}
	save := func() error { return miner.Checkpoint(path, report) }
	output := f.output
	if output == "" {
		output = filepath.Join(filepath.Dir(path), "dataset")
	}
	export := func() (string, error) {
		result, err := miner.Export(report, output, f.export)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s (%d training, %d validation)", result.Directory, result.Training, result.Validation), nil
	}
	switch action {
	case "judge":
		err = c.judge(ctx, path, report, f.callTimeout, save)
	case "review":
		if c.review == nil {
			return errors.New("interactive review is unavailable")
		}
		err = c.review(ctx, ui.MiningReviewConfig{Report: report, Save: save, Export: export, Input: c.input, Output: c.output})
	case "export":
		var result string
		result, err = export()
		if err == nil {
			fmt.Fprintln(c.output, "Exported:", safe(result))
		}
	}
	return errors.Join(err, writeMiningSummary(path, report))
}

func (c miningCommand) judge(ctx context.Context, path string, report *miner.Report, timeout time.Duration, save func() error) error {
	model := config.JoinModel(c.config.Provider, c.config.Model)
	if report.Judge != "" && report.Judge != model {
		return fmt.Errorf("report uses judge %s; select that model to resume, or scan a new report", safe(report.Judge))
	}
	pending := 0
	for _, candidate := range report.Candidates {
		if candidate.Assessment == nil && candidate.Review.Decision != "reject" && candidate.Review.Decision != "defer" {
			pending++
		}
	}
	if pending == 0 {
		fmt.Fprintln(c.output, "No open, unjudged candidates. Existing assessments and decisions were kept.")
		return nil
	}
	if c.makeJudge == nil {
		return errors.New("judge factory is unavailable")
	}
	fmt.Fprintf(c.output, "Judging %d candidates with %s. Candidate transcripts and feedback are sent to this provider; no tools are available.\n", pending, safe(model))
	factoryCtx, cancel := context.WithTimeout(ctx, timeout)
	modelClient, err := c.makeJudge(factoryCtx, c.config)
	cancel()
	if err != nil {
		return err
	}
	report.Judge = model
	if err := save(); err != nil {
		return err
	}
	err = miner.Judge(ctx, report, modelClient, timeout, save, func(done, total int) {
		fmt.Fprintf(c.output, "Judged %d/%d\n", done, total)
	})

	open, excluded := 0, 0
	for _, candidate := range report.Candidates {
		switch {
		case candidate.Review.Decision != "":
		case miner.Excluded(candidate):
			excluded++
		default:
			open++
		}
	}
	fmt.Fprintf(c.output, "Open for review: %d. Excluded by the judge's flags, hidden in review: %d.\n", open, excluded)
	fmt.Fprintf(c.output, "Report: %s\nJudge usage: %d input, %d output tokens.\nNext: julia mine review %q\n", safe(path), report.Usage.Prompt(), report.Usage.Output, path)
	return err
}

// The JSON report is the checkpoint. Markdown is a derived view, refreshed after
// each command, so a summary-write failure cannot roll back a saved human vote.
func writeMiningSummary(reportPath string, report *miner.Report) error {
	path := strings.TrimSuffix(reportPath, filepath.Ext(reportPath)) + ".md"
	if path == reportPath {
		return errors.New("report and summary paths must differ")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("summary destination must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mining-summary-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.WriteString(f, miner.Markdown(report)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func lockMiningReport(path string) (func(), error) {
	lockPath := path + ".lock"
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) {
		file.Close()
		return nil, errors.New("invalid report lock file")
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("report is in use by another mining command")
	}
	return func() { _ = file.Close() }, nil
}
