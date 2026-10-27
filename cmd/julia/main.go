package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"julia/agents"
	"julia/internal/browser"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/instructions"
	"julia/internal/memory"
	"julia/internal/provider"
	"julia/internal/session"
	"julia/internal/throughput"
	"julia/internal/tools"
	"julia/internal/ui"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "julia:", safe(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	// A closed terminal window sends SIGHUP; its default would exit at once,
	// before the running task's terminals and browser are closed, leaving
	// them running with nobody to stop them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	hangup, stopHangup := signal.NotifyContext(context.Background(), syscall.SIGHUP)
	defer stopHangup()
	configPath, err := config.Path()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("julia", flag.ContinueOnError)
	var path, profile, dir, prompt, modelFlag, reasoning, mode, resume, timeout, memoryFlag string
	var cont, demo, jsonMode, showVersion bool
	var snapshot int
	flags.StringVar(&path, "config", configPath, "configuration file")
	flags.StringVar(&profile, "profile", "", "configuration profile: config.<name>.json beside config.json, which is the default profile")
	flags.StringVar(&dir, "dir", ".", "workspace directory")
	flags.StringVar(&prompt, "prompt", "", "run one task without the TUI")
	flags.StringVar(&modelFlag, "model", "", "provider/<model-id>: codex, claude, openai, anthropic, google or the id of one of your providers (bare codex uses your Codex model, bare claude Claude Code's default)")
	flags.StringVar(&reasoning, "reasoning", "", "Reasoning effort (Codex model-supported value, Claude Code's low to max, or reasoning_effort for your providers; model support varies)")
	flags.StringVar(&mode, "mode", "", "auto, ask, read-only, or yolo (no sandbox, no review: for an unattended container)")
	flags.StringVar(&resume, "resume", "", "resume a saved session ID")
	flags.StringVar(&timeout, "timeout", "", "maximum duration per task, e.g. 4h")
	flags.StringVar(&memoryFlag, "memory", "", "off, read or write: whether notes kept between sessions are seen and saved (default write; read for a headless --prompt run)")
	flags.BoolVar(&cont, "continue", false, "resume latest session in this directory")
	flags.BoolVar(&demo, "demo", false, "explore the interface offline")
	flags.BoolVar(&jsonMode, "json", false, "emit JSON events in headless mode")
	flags.BoolVar(&showVersion, "version", false, "print version")
	flags.IntVar(&snapshot, "snapshot", 0, "render an offline demo at this terminal width")
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), "JULIA · a quiet interface for ambitious work\n\nUsage: julia [flags]\n       julia doctor | login | sessions | config init\n")
		flags.PrintDefaults()
	}
	if err = flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if showVersion {
		fmt.Println("julia", version)
		return nil
	}
	configSet := false
	flags.Visit(func(f *flag.Flag) { configSet = configSet || f.Name == "config" })
	initializing := slices.Equal(flags.Args(), []string{"config", "init"})
	var cfg config.Config
	switch {
	case profile != "" && configSet:
		return errors.New("choose either --config or --profile")
	case profile != "" && !config.ValidProfileName(profile):
		return fmt.Errorf("--profile %q is not a profile name, which is %s", profile, config.ProfileRule)
	case profile != "":
		path = config.ProfilePath(filepath.Dir(configPath), profile)
		if initializing {
			// The profile is what config init creates.
			cfg, err = config.Load(path)
		} else {
			cfg, err = config.LoadProfile(filepath.Dir(configPath), profile)
		}
	default:
		cfg, err = config.Load(path)
	}
	if err != nil {
		return err
	}
	// Profiles are the configuration files beside this one; see config.Profiles.
	configDir := filepath.Dir(path)
	// The "model" entry's reasoning belongs to that provider and model only.
	backend, model := config.SplitModel(modelFlag)
	if modelFlag != "" && (backend != cfg.Provider || model != cfg.Model) {
		cfg.Provider, cfg.Model, cfg.ModelReasoning = backend, model, ""
	}
	reasoningSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "reasoning" {
			reasoningSet = true
			cfg.Reasoning = reasoning
		}
	})
	overrides := launchOverrides{mode: mode, timeout: timeout}
	overrides.apply(&cfg)
	// The model's own configured reasoning, else the last effort chosen for
	// this provider, replaces the configured default; an explicit flag is
	// itself a choice and becomes the new default.
	preferencesPath, preferencesErr := config.PreferencesPath()
	if !reasoningSet {
		var prefs config.Preferences
		if preferencesErr == nil {
			prefs, _ = config.LoadPreferences(preferencesPath)
		}
		cfg.ApplyStartingReasoning(prefs)
	}
	if err = cfg.Validate(); err != nil {
		return err
	}
	if reasoningSet && preferencesErr == nil && !demo && snapshot == 0 {
		_ = config.RememberReasoning(preferencesPath, cfg.Provider, cfg.Reasoning)
	}
	roles, err := loadRoles(path, cfg)
	if err != nil {
		return err
	}
	store, err := session.Default()
	if err != nil {
		return err
	}
	defer store.Close()
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return errors.New("workspace must be a directory")
	}
	args := flags.Args()
	// Consolidation is a headless task of the agent's: a pass over the notes
	// with the Memory tool and nothing else, so the workspace tools run
	// read-only while notes are written, and no reflection follows it.
	consolidate := false
	if len(args) > 0 {
		switch args[0] {
		case "doctor":
			return doctor(ctx, cfg, path, roles, dir)
		case "memory":
			if len(args) != 2 || args[1] != "consolidate" {
				return memoryCommand(args[1:], dir, os.Stdout)
			}
			consolidate = true
			prompt, memoryFlag, cfg.Mode = memory.ConsolidationPrompt, "write", "read-only"
		case "login":
			return login(ctx, cfg, args[1:])
		case "config":
			if len(args) != 2 || args[1] != "init" {
				return errors.New("usage: julia config init")
			}
			if err = config.Init(path); err != nil {
				return err
			}
			fmt.Println("Created", path)
			return nil
		case "sessions":
			list, err := store.List()
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
		default:
			return fmt.Errorf("unknown command %q (use --prompt for a headless task)", args[0])
		}
	}
	linearToken, err := tools.LoadLinearToken(linearTokenFile(path))
	if err != nil {
		return fmt.Errorf("linear token: %w", err)
	}
	mailConfig, err := tools.LoadMailConfig(mailConfigFile(path))
	if err != nil {
		return fmt.Errorf("mail config: %w", err)
	}
	// Notes are read in every run and written where someone watches: a
	// headless task has no one to see what it saves, or to approve a save
	// that follows content from outside the user's words, so it learns by
	// reporting unless told otherwise.
	if memoryFlag == "" {
		memoryFlag = "write"
		if prompt != "" {
			memoryFlag = "read"
		}
	}
	var memoryStore *memory.Store
	switch memoryFlag {
	case "off":
	case "read", "write":
		if memoryStore, err = memory.Default(); err != nil {
			return err
		}
	default:
		return errors.New("--memory must be off, read or write")
	}
	demoTools := append(tools.Definitions(roles, linearToken != "", mailConfig.Enabled()), tools.QuestionDefinition(), tools.ReviewDefinition())
	if snapshot > 0 {
		fmt.Print(ui.RenderDemo(snapshot, 40, demoTools...))
		return nil
	}
	s := session.New(dir, cfg.Provider, cfg.Model)
	if resume != "" && cont {
		return errors.New("choose either --resume or --continue")
	}
	if resume != "" {
		s, err = store.Load(resume)
	} else if cont {
		s, err = store.Latest(dir)
	}
	if err != nil {
		return err
	}
	if demo {
		if err := selectSessionModel(&cfg, s, dir, backend, model, reasoningSet, resume != "" || cont); err != nil {
			return err
		}
		preview := baseUIConfig(s, cfg, demoTools)
		preview.Demo, preview.Profile = true, profileLabel(path)
		return ui.Run(preview)
	}
	if err := store.Lock(s.ID); err != nil {
		return err
	}
	if resume != "" || cont {
		// Re-read only after locking: another process may have completed between
		// listing/loading the session and acquiring exclusive ownership.
		s, err = store.Load(s.ID)
		if err != nil {
			return err
		}
	}
	if err := selectSessionModel(&cfg, s, dir, backend, model, reasoningSet, resume != "" || cont); err != nil {
		return err
	}
	if prompt == "" && !term.IsTerminal(os.Stdin.Fd()) {
		return errors.New("interactive mode requires a terminal; use --prompt for headless work")
	}
	factory := func(ctx context.Context, selected config.Config) (core.Backend, core.Approver, error) {
		return newBackend(ctx, selected, store.Save)
	}
	raw, review, err := factory(ctx, cfg)
	if err != nil {
		return err
	}
	approvals := make(chan ui.ApprovalRequest)
	approve := func(ctx context.Context, a core.Approval) (core.Decision, error) {
		if prompt != "" {
			return core.Decision{}, fmt.Errorf("human approval required: %s; resume in the TUI", a.Reason)
		}
		reply := make(chan core.Decision, 1)
		select {
		case approvals <- ui.ApprovalRequest{Approval: a, Reply: reply}:
		case <-ctx.Done():
			return core.Decision{}, ctx.Err()
		}
		select {
		case d := <-reply:
			return d, nil
		case <-ctx.Done():
			return core.Decision{}, ctx.Err()
		}
	}
	// The tool is registered in every run so a session's tool catalog is the same
	// in the TUI and headless; with nobody to answer, a headless question is skipped.
	ask, triage, questions := headlessAsker, headlessTriager, (<-chan ui.QuestionRequest)(nil)
	if prompt == "" {
		ask, triage, questions = ui.NewBridge()
	}
	duration, _ := time.ParseDuration(cfg.Timeout)
	runner := &runtimeBackend{backend: raw, cfg: cfg, roles: roles, approve: approve, review: review, ask: ask, triage: triage, makeBackend: factory, timeout: duration, parent: ctx, headless: prompt != "", checkpoint: store.Save, searchKeyFile: searchKeyFile(path), linearToken: linearToken, mail: mailConfig, memory: memoryStore, memoryReadOnly: memoryFlag == "read", consolidating: consolidate}
	if consolidate {
		// The pass rewrites notes and nothing else: no one is asked, and a
		// note it rewrites keeps its unverified mark, as a yolo save would.
		runner.memoryMode = "yolo"
	}
	// Every timed model call feeds the speed shown beside each model in the picker.
	throughputPath, throughputErr := throughput.Path()
	if throughputErr == nil && !demo && snapshot == 0 {
		runner.throughputPath = throughputPath
	}
	if prompt != "" {
		s.Title = session.Title(prompt)
		if consolidate {
			s.Title = "Consolidate memory"
			// Not a conversation to continue: --continue skips it.
			s.Role = "consolidation"
		}
		enc := json.NewEncoder(os.Stdout)
		emit := func(e core.Event) {
			if jsonMode {
				_ = enc.Encode(e)
				return
			}
			if e.Subagent != nil {
				// A child's text is its report to the parent, not output; its
				// tool calls show as progress under the role's name.
				if e.Kind == "tool_start" && e.Call != nil {
					fmt.Fprintln(os.Stderr, "  ›", safe(e.Subagent.Role), "›", e.Call.Name)
				}
				return
			}
			switch e.Kind {
			case "text":
				fmt.Print(safe(e.Text))
			case "tool_start":
				fmt.Fprintln(os.Stderr, "  ›", e.Call.Name)
			case "warning", "status":
				fmt.Fprintln(os.Stderr, "  ·", safe(e.Text))
			}
		}
		err = runner.Run(ctx, s, core.Prompt{Text: prompt}, nil, emit)
		saveErr := store.Save(s)
		if jsonMode {
			msg := "completed"
			if err != nil {
				msg = "failed"
			}
			_ = enc.Encode(map[string]any{"kind": "done", "status": msg, "session_id": s.ID})
		} else {
			fmt.Println()
			fmt.Fprintln(os.Stderr, "Session:", s.ID)
			fmt.Fprintln(os.Stderr, "Tokens:", tokenSummary(s.Usage.Total))
		}
		return errors.Join(err, saveErr)
	}
	uiConfig := baseUIConfig(s, cfg, runner.Definitions())
	uiConfig.Backend, uiConfig.Save, uiConfig.Approvals, uiConfig.Questions = runner, store.Save, approvals, questions
	if memoryStore != nil {
		uiConfig.Memory = func(action, name string) (string, error) {
			// The session's workspace, which a resumed session may not share with the current directory.
			return memoryMarkdown(memoryStore, s.Directory, action, name, memoryFlag == "read")
		}
	}
	uiConfig.Hangup = hangup
	// The reflection on a turn runs after it, telling the interface as it goes.
	followups := make(chan ui.Followup)
	runner.followups, runner.closed, uiConfig.Followups = followups, make(chan struct{}), followups
	uiConfig.Children = func(parent string) ([]*core.Session, error) {
		all, err := store.List()
		if err != nil {
			return nil, err
		}
		var children []*core.Session
		for _, child := range all {
			if child.ParentID == parent {
				children = append(children, child)
			}
		}
		return children, nil
	}
	// The gauge follows the session's model: a model with a context_window of
	// its own is measured against it, in whichever profile is active.
	uiConfig.ContextWindowFor = func(provider, model string) int {
		cfg, _ := runner.settings()
		return cfg.ContextWindowFor(provider, model)
	}
	uiConfig.ApplySettings = func(current *core.Session, selectedProvider, selectedModel, reasoning string) error {
		cfg, _ := runner.settings()
		if !config.IsCLIProvider(selectedProvider) {
			if _, err := apiKey(cfg, selectedProvider); err != nil {
				return err
			}
		}
		previousProvider := current.Provider
		if err := applySessionSettings(cfg, current, selectedProvider, selectedModel, reasoning, store.Save); err != nil {
			return err
		}
		// A provider switch resets the effort rather than choosing one, so it must
		// not overwrite the destination provider's remembered choice.
		if selectedProvider == previousProvider && preferencesErr == nil {
			_ = config.RememberReasoning(preferencesPath, current.Provider, current.Reasoning)
		}
		return nil
	}
	settings := profileSettings(cfg)
	uiConfig.EnabledModels, uiConfig.NamedProviders = settings.EnabledModels, settings.NamedProviders
	// Switching profile replaces the runner's configuration between turns;
	// the launch's --mode and --timeout hold in every profile.
	uiConfig.Profile = profileLabel(path)
	uiConfig.Profiles = func() ([]ui.Profile, error) { return profileChoices(configDir) }
	uiConfig.SwitchProfile = func(current *core.Session, name string) (ui.ProfileSettings, error) {
		prefs := ""
		if preferencesErr == nil {
			prefs = preferencesPath
		}
		next, err := loadSwitchedProfile(configDir, name, overrides, prefs)
		if err != nil {
			return ui.ProfileSettings{}, err
		}
		if err := applySessionSettings(next, current, next.Provider, next.Model, next.Reasoning, store.Save); err != nil {
			return ui.ProfileSettings{}, err
		}
		runner.switchProfile(next)
		return profileSettings(next), nil
	}
	// The CLIs are found through the active profile's codex_binary and
	// claude_binary.
	active := func() config.Config {
		cfg, _ := runner.settings()
		return cfg
	}
	uiConfig.Models = func(ctx context.Context) ([]core.ModelOption, error) {
		return cliCatalog(ctx, active())
	}
	if throughputErr == nil {
		uiConfig.Throughput = func() map[string]throughput.Stats {
			log, _ := throughput.Load(throughputPath)
			return log.All()
		}
	}
	uiConfig.WeeklyUsage = func(ctx context.Context) (*core.WeeklyUsage, error) {
		cfg := active()
		if cfg.Provider == "claude" {
			binary, err := findClaude(cfg.ClaudeBinary)
			if err != nil {
				return nil, err
			}
			return (&provider.Claude{Binary: binary}).WeeklyUsage(ctx)
		}
		binary, err := findCodex(cfg.CodexBinary)
		if err != nil {
			return nil, err
		}
		return (&provider.Codex{Binary: binary}).WeeklyUsage(ctx)
	}
	runErr := ui.Run(uiConfig)
	pending := runner.finish(ctx, func() {
		fmt.Fprintln(os.Stderr, "julia: finishing the reflection on your last turn · Ctrl+C skips it")
	})
	keepErr := keepAfterTurns(pending, s, store)
	if runErr == nil {
		width, _, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			width = 80
		}
		lipgloss.Fprint(os.Stdout, ui.RenderGoodbye(s, resumeCommand(s), width))
	}
	return errors.Join(runErr, keepErr)
}

// resumeCommand is the command that reopens s from the shell the interface
// was launched in, naming the workspace only when that shell is not in it.
func resumeCommand(s *core.Session) string {
	command := "julia --resume " + s.ID
	if wd, err := os.Getwd(); err == nil {
		if resolved, err := filepath.EvalSymlinks(wd); err == nil {
			wd = resolved
		}
		if wd == s.Directory {
			return command
		}
	}
	return command + " --dir " + shellQuote(s.Directory)
}

// shellQuote leaves a plain path as it is and single-quotes anything else.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Resolve launch settings after locking and rereading a resumed session. An
// explicit Codex model override is safe because thread/resume accepts it; API
// histories retain their selected model until provider-specific migration exists.
func selectSessionModel(cfg *config.Config, s *core.Session, directory, providerOverride, modelOverride string, reasoningOverride, resuming bool) error {
	if resuming {
		if s.ParentID != "" {
			return errors.New("subagent sessions are transcripts only; resume the parent session")
		}
		if s.Directory != directory {
			return fmt.Errorf("session belongs to %s; use --dir with that workspace", s.Directory)
		}
		if providerOverride != "" && providerOverride != s.Provider {
			return errors.New("resuming with a different provider is not supported; start a new session")
		}
		if modelOverride != "" && modelOverride != s.Model {
			if !config.IsCLIProvider(s.Provider) {
				return errors.New("resuming with a different API model is not supported; start a new session")
			}
			s.Model = modelOverride
		}
		cfg.Provider, cfg.Model = s.Provider, s.Model
		if !reasoningOverride && (s.ReasoningSet || cfg.Reasoning == "") {
			cfg.Reasoning = s.Reasoning
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.Reasoning = cfg.Reasoning
	s.ReasoningSet = s.ReasoningSet || reasoningOverride
	return nil
}

func modelSettings(cfg config.Config) (model, reasoning string) {
	model, reasoning = cfg.Model, cfg.Reasoning
	if model == "" {
		model = "provider default"
		if config.IsCLIProvider(cfg.Provider) {
			model = "inherited from " + config.CLIProviderName(cfg.Provider)
		}
	}
	if reasoning == "" {
		reasoning = "not configurable for this provider"
		if config.IsCLIProvider(cfg.Provider) {
			reasoning = "inherited from " + config.CLIProviderName(cfg.Provider)
		} else if cfg.SupportsReasoning(cfg.Provider) {
			reasoning = "provider/model default"
		}
	}
	return model, reasoning
}

type runtimeBackend struct {
	headless    bool
	checkpoint  func(*core.Session) error
	backend     core.Backend
	makeBackend func(context.Context, config.Config) (core.Backend, core.Approver, error)
	// cfg and timeout are the active profile's, read through settings:
	// switching profile replaces them between turns, while a reflection on
	// an earlier turn may still be running.
	cfg             config.Config
	timeout         time.Duration
	profileMu       sync.Mutex
	approve, review core.Approver
	ask             core.Asker
	triage          core.Triager
	parent          context.Context
	// searchKeyFile holds the Firecrawl key saved by WebSearch's first-use setup.
	searchKeyFile string
	// linearToken is the Linear access token, from the environment or the
	// linear.json file beside the configuration; empty offers no Linear tool.
	linearToken string
	// mail is the mailbox from the mail.json file beside the configuration;
	// without credentials there is no Mail tool.
	mail tools.MailConfig
	// throughputPath is where timed model calls are recorded; empty records none.
	throughputPath string
	throughputMu   sync.Mutex
	// memory holds the notes kept between sessions; nil keeps none and
	// offers no Memory tool. memoryReadOnly lets a run see notes but save
	// none, and memoryMode, when set, is the approval mode the Memory tool
	// uses instead of the configuration's. consolidating marks the run
	// that is itself the consolidation pass, which no reflection follows.
	memory         *memory.Store
	memoryReadOnly bool
	memoryMode     string
	consolidating  bool
	// followups carries the reflection that runs after its turn has ended
	// to the interface; nil reflects within the turn, as a headless run
	// does. closed ends the carrying once the interface has gone (see
	// finish), and afterTurns hold, by session, what reflections after a
	// turn changed until the session takes it in.
	followups  chan<- ui.Followup
	closed     chan struct{}
	afterMu    sync.Mutex
	afterTurns map[string][]*afterTurn
	// roles are the subagent roles a delegated task may name; children are run
	// by makeBackend, so without it there are none.
	roles []agents.Role
}

// settings are the configuration and task timeout a turn starts with.
func (r *runtimeBackend) settings() (config.Config, time.Duration) {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	return r.cfg, r.timeout
}

// switchProfile makes cfg, a validated configuration, the one the next
// turns run on.
func (r *runtimeBackend) switchProfile(cfg config.Config) {
	timeout, _ := time.ParseDuration(cfg.Timeout)
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	r.cfg, r.timeout = cfg, timeout
}

// recordThroughput logs the speed of a timed model call under the session's
// model, which a Codex session learns from its thread, so the picker can show
// it. An aggregate event sums a child's calls, each already logged by the child.
func (r *runtimeBackend) recordThroughput(s *core.Session, event core.Event) {
	if r.throughputPath == "" || event.Kind != "usage" || event.Aggregate || event.Elapsed <= 0 || event.OutputTokens <= 0 || s.Model == "" {
		return
	}
	r.throughputMu.Lock()
	defer r.throughputMu.Unlock()
	_ = throughput.Add(r.throughputPath, config.JoinModel(s.Provider, s.Model), throughput.Sample{At: time.Now(), Output: event.OutputTokens, Seconds: event.Elapsed.Seconds()})
}

// headlessAsker answers for a run with no human: it skips the question at once.
func headlessAsker(context.Context, core.Question) (core.Answer, error) {
	return core.Answer{Skipped: true}, nil
}

// headlessTriager skips a review the same way, so the model reports its findings.
func headlessTriager(context.Context, core.Review) (core.Answer, error) {
	return core.Answer{Skipped: true}, nil
}

func (r *runtimeBackend) Definitions() []core.ToolDefinition {
	definitions := tools.Definitions(r.subagentRoles(), r.linearToken != "", r.mail.Enabled())
	if r.ask != nil {
		definitions = append(definitions, tools.QuestionDefinition())
	}
	if r.triage != nil {
		definitions = append(definitions, tools.ReviewDefinition())
	}
	if r.memory != nil {
		definitions = append(definitions, tools.MemoryDefinition())
	}
	return definitions
}

// subagentRoles are the roles the Subagent tool offers, none when children
// cannot be run.
func (r *runtimeBackend) subagentRoles() []agents.Role {
	if r.makeBackend == nil {
		return nil
	}
	return r.roles
}

func (r *runtimeBackend) Name() string { return r.backend.Name() }
func (r *runtimeBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, _ core.ToolExecutor, emit core.Emit) error {
	// The turn runs on the profile it started with.
	cfg, timeout := r.settings()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(r.parent, cancel)
	defer stop()
	selected := cfg
	selected.Provider, selected.Model, selected.Reasoning = s.Provider, s.Model, s.Reasoning
	backend, review := r.backend, r.review
	if r.makeBackend != nil {
		var err error
		backend, review, err = r.makeBackend(ctx, selected)
		if err != nil {
			return err
		}
	}
	var guard core.Approver
	if review != nil {
		guard = func(ctx context.Context, approval core.Approval) (core.Decision, error) {
			reviewCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			return review(reviewCtx, approval)
		}
	}
	consumer := emit
	var emitMu sync.Mutex
	// The turn's warnings, denials among them, are what the reflection step
	// reads besides the transcript. recovered marks a turn in which a tool
	// failed and the work went on: what made it go on is often the lesson.
	var turnWarnings []string
	failed, recovered := false, false
	emit = func(event core.Event) {
		if event.Subagent == nil {
			r.recordThroughput(s, event)
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		// A child's events are audited in the child's session, not here.
		if event.Subagent == nil && (event.Kind == "tool_start" || event.Kind == "tool_end" || event.Kind == "warning" || event.Kind == "status") {
			s.Audit = append(s.Audit, core.AuditEntry{At: time.Now(), Event: event})
		}
		if event.Subagent == nil && event.Kind == "warning" {
			turnWarnings = append(turnWarnings, event.Text)
		}
		if event.Subagent == nil && event.Kind == "tool_end" && event.Result != nil && event.Result.IsError {
			failed = true
		} else if event.Subagent == nil && event.Kind == "tool_start" && failed {
			recovered = true
		}
		consumer(event)
	}
	// What reflections on earlier turns changed joins the session first, so
	// the transcript keeps its order and this turn sees their notes.
	r.settleEarlier(ctx, s, emit)
	if err := ctx.Err(); err != nil {
		return err
	}
	task := newTaskContext(s.Messages, prompt.Text)
	executor, err := tools.New(tools.Config{Directory: s.Directory, Mode: cfg.Mode, Approve: r.approve, Review: guard, Task: task.String(), Emit: emit, SearchKey: os.Getenv(tools.WebSearchKeyEnv), SearchKeyFile: r.searchKeyFile, ScratchDir: filepath.Join(os.TempDir(), "julia-scratch", s.ID), Browser: cfg.BrowserPath(), SlackToken: os.Getenv(tools.SlackTokenEnv), SlackOwner: os.Getenv(tools.SlackOwnerEnv), LinearToken: r.linearToken, Mail: r.mail})
	if err != nil {
		return err
	}
	defer executor.Close()
	goEnv, environmentErr := executor.EnvironmentInfo(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if environmentErr != nil {
		emit(core.Event{Kind: "warning", Text: "Go environment preparation: " + environmentErr.Error()})
	}
	bashEnvironment := make(map[string]string)
	for key, value := range map[string]string{"GOROOT": goEnv.GoRoot, "GOCACHE": goEnv.GoBuildCache, "GOMODCACHE": goEnv.GoModuleCache} {
		if value != "" {
			bashEnvironment[key] = value
		}
	}
	// What earlier sessions learned joins the instructions: how to learn,
	// then the index of notes, with the ones the request names shown in full.
	// An unverified note shown in full brings its outside content with it.
	memoryIndex := ""
	if r.memory != nil {
		if recall, err := r.memory.Recall(s.Directory); err != nil {
			emit(core.Event{Kind: "warning", Text: "Memory not loaded this turn: " + err.Error()})
		} else {
			for _, warning := range recall.Warnings {
				emit(core.Event{Kind: "warning", Text: warning})
			}
			// An unverified note is labelled wherever it is shown; only reading
			// one in full on purpose taints the conversation, or a single such
			// note with a common keyword would mark every session it reaches.
			memoryIndex = recall.Section(cfg.Memory.IndexBytes, prompt.Text, r.memoryReadOnly)
			relevant := recall.Relevant(prompt.Text)
			if n := recall.Count(); n > 0 {
				text := fmt.Sprintf("Recalled %d %s", n, map[bool]string{true: "note", false: "notes"}[n == 1])
				if len(relevant) > 0 {
					text += fmt.Sprintf(" · %d relevant now", len(relevant))
				}
				emit(core.Event{Kind: "status", Text: text})
			}
			// The consolidation pass sees every note to rewrite it, not to use it.
			if !r.consolidating {
				turn := memory.Activity{Kind: memory.ActivityTurn, Session: s.ID, Workspace: s.Directory, Notes: recall.Count(), Omitted: recall.Omitted(cfg.Memory.IndexBytes, prompt.Text)}
				for _, note := range relevant {
					turn.Relevant = append(turn.Relevant, note.Name)
				}
				_ = r.memory.Log(turn)
			}
		}
	}
	bundle, err := instructions.Build(instructions.Options{
		Directory: s.Directory, Provider: s.Provider, Model: s.Model, Reasoning: selected.Reasoning, Mode: cfg.Mode,
		Sandbox: executor.SandboxInfo(), Timeout: timeout.String(), Headless: r.headless,
		SkillPaths: cfg.SkillPaths, CodexHome: os.Getenv("CODEX_HOME"),
		BashEnvironment: bashEnvironment, WorkspaceInstructions: cfg.WorkspaceInstructions,
		Memory: memoryIndex,
	})
	if err != nil {
		return fmt.Errorf("load agent instructions: %w", err)
	}
	s.Instructions = bundle.Text
	for _, warning := range bundle.Warnings {
		emit(core.Event{Kind: "warning", Text: warning})
	}
	for _, path := range bundle.Skills {
		emit(core.Event{Kind: "status", Text: "Loaded skill: " + path})
	}
	for _, path := range bundle.Repository {
		emit(core.Event{Kind: "status", Text: "Loaded repository instructions: " + path})
	}
	if s.Title == "New conversation" || s.Title == "" {
		s.Title = session.Title(prompt.Text)
	}
	var agentTools core.ToolExecutor = executor
	if roles := r.subagentRoles(); len(roles) > 0 {
		// Snapshot trusted settings before the parent provider changes its session.
		parentID, directory := s.ID, s.Directory
		agentTools = tools.WithSubagents(executor, roles, func(childCtx context.Context, role agents.Role, task string, childTools core.ToolExecutor) (string, error) {
			settings := subagentSettings(cfg, selected, role.Name)
			modelID := config.JoinModel(settings.Provider, settings.Model)
			instructionText, err := bundle.For(settings.Provider, settings.Model, settings.Reasoning)
			if err != nil {
				return "", err
			}
			// A child that cannot run is refused before it is saved or announced.
			childBackend, _, err := r.makeBackend(childCtx, settings)
			if err != nil {
				return "", fmt.Errorf("%s: %w", modelID, err)
			}
			child := session.New(directory, settings.Provider, settings.Model)
			child.ParentID = parentID
			child.Role = role.Name
			child.Title = "Subagent " + role.Name + ": " + session.Title(task)
			child.Reasoning, child.ReasoningSet = settings.Reasoning, true
			child.Instructions = instructionText + subagentInstructions(role, r.memoryState())
			if r.checkpoint != nil {
				if err := r.checkpoint(child); err != nil {
					return "", err
				}
			}
			emit(core.Event{Kind: "status", Text: "Started " + role.Name + " on " + modelID + " · subagent " + child.ID})
			// The child's events join the parent stream marked as the child's,
			// so the interface can show the fleet at work; the child session
			// audits them, as the parent's audits its own.
			ref := &core.Subagent{ID: child.ID, Role: role.Name, Model: modelID}
			emit(core.Event{Kind: "subagent_start", Text: task, Subagent: ref})
			childEmit := func(event core.Event) {
				r.recordThroughput(child, event)
				if event.Kind == "tool_start" || event.Kind == "tool_end" || event.Kind == "warning" || event.Kind == "status" {
					child.Audit = append(child.Audit, core.AuditEntry{At: time.Now(), Event: event})
				}
				event.Subagent = ref
				emit(event)
			}
			err = childBackend.Run(childCtx, child, core.Prompt{Text: task}, childTools, childEmit)
			ended := core.Event{Kind: "subagent_end", Subagent: ref}
			if err != nil {
				ended.Text = err.Error()
			}
			emit(ended)
			if r.checkpoint != nil {
				err = errors.Join(err, r.checkpoint(child))
			}
			// The child's provider records into the child session only, and its events stay
			// out of the parent stream, so its spend reaches the parent exactly once, here,
			// even when the child failed. Children run concurrently, hence emitMu. Context
			// stays untouched: the child's conversation is not part of the parent's. The
			// provider checkpoints the parent when this tool returns, which persists it.
			if spent := child.Usage.Total; !spent.IsZero() {
				event := core.AggregateUsageEvent(spent)
				emitMu.Lock()
				s.RecordUsage(event)
				emitMu.Unlock()
				emit(event)
			}
			status := "Completed "
			if err != nil {
				status = "Failed "
			}
			emit(core.Event{Kind: "status", Text: status + role.Name + " · subagent " + child.ID})
			output := "Child session: " + child.ID + " (" + role.Name + " on " + modelID + ")\n"
			for i := len(child.Messages) - 1; i >= 0; i-- {
				if child.Messages[i].Role == "assistant" && child.Messages[i].Content != "" {
					output += child.Messages[i].Content
					break
				}
			}
			return output, err
		})
	}
	// Outermost: subagent children receive the base executor and so can never ask the user.
	agentTools = tools.WithInteractions(agentTools, r.ask, r.triage)
	if r.ask != nil || r.triage != nil {
		// The user's answers are their own words: later approvals this turn are judged with them in view.
		agentTools = withAnswerContext(agentTools, task, executor.SetTask)
	}
	// Outermost of all: the Memory tool sees every call, so it knows what the
	// turn read before it saves, and children, who get the base executor,
	// never save on their own.
	var memoryTools *tools.Memory
	if r.memory != nil {
		mode := r.memoryMode
		if mode == "" {
			mode = cfg.Mode
		}
		// Headless, nobody can approve: the tool then refuses with a reason
		// that tells the model to report instead.
		approveMemory := r.approve
		if r.headless {
			approveMemory = nil
		}
		// The session carries the taint the moment it grows, so a checkpoint
		// taken mid-turn, or a crash, cannot drop it.
		onTaint := func(taint []string) {
			emitMu.Lock()
			s.MemoryTaint = taint
			emitMu.Unlock()
		}
		onDeliver := func(delivered []string) {
			emitMu.Lock()
			s.MemoryDelivered = delivered
			emitMu.Unlock()
		}
		if r.consolidating {
			// The pass rewrites notes and nothing else.
			agentTools = onlyMemory{agentTools}
		}
		memoryTools = tools.WithMemory(agentTools, tools.MemoryOptions{Store: r.memory, Workspace: s.Directory, Session: s.ID, Mode: mode, WorkspaceMode: cfg.Mode, ReadOnly: r.memoryReadOnly, Unbounded: r.consolidating, Approve: approveMemory, Task: task.String, Tainted: s.MemoryTaint, OnTaint: onTaint, Delivered: s.MemoryDelivered, OnDeliver: onDeliver})
		agentTools = memoryTools
	}
	if prompt.Steering != nil {
		// What the user sends mid-turn is theirs, as the prompt is: later
		// approvals this turn are judged with it in view.
		prompt.Steering = steeredTask{Steering: prompt.Steering, record: func(text string) { executor.SetTask(task.steered(text)) }}
	}
	start := len(s.Messages)
	err = backend.Run(ctx, s, prompt, agentTools, emit)
	if memoryTools != nil {
		// The model reasons from the whole conversation, so what this turn
		// read from outside the user's words weighs on every turn after it.
		emitMu.Lock()
		s.MemoryTaint = memoryTools.Tainted()
		emitMu.Unlock()
	}
	if r.reflects(ctx, cfg, err) {
		emitMu.Lock()
		warnings, wentOn := append([]string(nil), turnWarnings...), recovered
		emitMu.Unlock()
		if start >= len(s.Messages) || !worthReflecting(s.Messages[start:], warnings, wentOn) {
			_ = r.memory.Log(memory.Activity{Kind: memory.ActivityReflection, Session: s.ID, Workspace: s.Directory, Outcome: memory.ReflectionSkipped})
		} else {
			in := reflectionInput{session: s.ID, directory: s.Directory, messages: slices.Clone(s.Messages[start:]), start: start, after: len(s.Messages), warnings: warnings, tainted: slices.Clone(s.MemoryTaint), task: task.String(), selected: selected, backend: backend}
			// The reflection runs after the turn, unless its changes may
			// need the user, who can be asked only while the turn lasts: in
			// auto mode, after content from outside the user's words.
			if r.followups != nil && !(cfg.Mode == "auto" && len(in.tainted) > 0) {
				r.reflectLater(in)
			} else {
				in.canAsk = true
				out := r.reflection(ctx, in, func(string) {
					text := "Reflecting on this turn"
					if !r.headless {
						text += " · Esc skips"
					}
					emit(core.Event{Kind: "status", Text: text})
				})
				if !out.usage.IsZero() {
					emit(core.AggregateUsageEvent(out.usage))
				}
				for _, event := range out.events {
					emit(event)
				}
				settle(s, in.after, out, false)
			}
		}
	}
	return err
}

// memoryState is the run's memory as a child is told of it: off, read or write.
func (r *runtimeBackend) memoryState() string {
	switch {
	case r.memory == nil:
		return "off"
	case r.memoryReadOnly:
		return "read"
	}
	return "write"
}

// onlyMemory offers the consolidation pass the Memory tool alone, which
// WithMemory adds around it, and refuses any other call should the model
// make one anyway.
type onlyMemory struct{ base core.ToolExecutor }

func (onlyMemory) Definitions() []core.ToolDefinition { return nil }
func (o onlyMemory) Execute(_ context.Context, call core.ToolCall) core.ToolResult {
	return core.ToolResult{Content: "the consolidation pass uses the Memory tool alone; " + call.Name + " is unavailable", IsError: true}
}

// baseUIConfig is what the live and demo interfaces share. The context window
// comes from configuration so the sidebar gauge and API compaction measure the
// same budget.
func baseUIConfig(s *core.Session, cfg config.Config, definitions []core.ToolDefinition) ui.Config {
	return ui.Config{Session: s, Version: version, Mode: cfg.Mode, ToolDefinitions: definitions, ContextWindow: cfg.ContextTokens}
}

func findCodex(binary string) (string, error) {
	if path, err := exec.LookPath(binary); err == nil {
		return path, nil
	}
	if binary == "codex" {
		for _, path := range []string{"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex", "/Applications/Codex.app/Contents/Resources/codex"} {
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", errors.New("Codex CLI was not found; install it or set codex_binary in your config")
}

// findClaude resolves the Claude Code CLI: the configured binary on PATH, else
// where its native installer and npm put it.
func findClaude(binary string) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	if path, err := exec.LookPath(binary); err == nil {
		return path, nil
	}
	if binary == "claude" {
		home, _ := os.UserHomeDir()
		for _, path := range []string{filepath.Join(home, ".local", "bin", "claude"), filepath.Join(home, ".claude", "local", "claude"), "/opt/homebrew/bin/claude", "/usr/local/bin/claude"} {
			if home == "" && strings.HasPrefix(path, string(filepath.Separator)+".") {
				continue
			}
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", errors.New("Claude Code CLI was not found; install it or set claude_binary in your config")
}

// claudeLoginStatus reports Claude Code's sign-in as `claude auth status`
// prints it, without reading any credential.
func claudeLoginStatus(ctx context.Context, binary string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "auth", "status").Output()
	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err != nil || json.Unmarshal(out, &status) != nil {
		return "unknown · run claude auth status"
	}
	if !status.LoggedIn {
		return "signed out · run julia login claude"
	}
	return "signed in · " + safe(status.AuthMethod)
}

// login signs in to a CLI provider's own account through its CLI: the one
// named, else the configured provider's, else Codex.
func login(ctx context.Context, cfg config.Config, args []string) error {
	which := cfg.Provider
	if len(args) > 0 {
		which = args[0]
	}
	if !config.IsCLIProvider(which) {
		if len(args) > 0 {
			return errors.New("usage: julia login [codex|claude]")
		}
		which = "codex"
	}
	var cmd *exec.Cmd
	if which == "claude" {
		binary, err := findClaude(cfg.ClaudeBinary)
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, binary, "auth", "login")
	} else {
		cmd = exec.CommandContext(ctx, cfg.CodexBinary, "login")
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cliCatalog lists the models of every installed CLI provider. One that is
// missing or fails still leaves the others' models, with its error.
func cliCatalog(ctx context.Context, cfg config.Config) ([]core.ModelOption, error) {
	var models []core.ModelOption
	var errs []error
	if binary, err := findCodex(cfg.CodexBinary); err != nil {
		errs = append(errs, err)
	} else if catalog, err := (&provider.Codex{Binary: binary}).Models(ctx); err != nil {
		errs = append(errs, fmt.Errorf("Codex models: %w", err))
	} else {
		models = append(models, catalog...)
	}
	if binary, err := findClaude(cfg.ClaudeBinary); err != nil {
		errs = append(errs, err)
	} else if catalog, err := (&provider.Claude{Binary: binary}).Models(ctx); err != nil {
		errs = append(errs, fmt.Errorf("Claude Code models: %w", err))
	} else {
		models = append(models, catalog...)
	}
	if len(models) == 0 {
		return nil, errors.Join(errs...)
	}
	return models, nil
}

// searchKeyFile keeps the Firecrawl key beside the configuration file it serves.
func searchKeyFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "firecrawl.json")
}

// linearTokenFile keeps the Linear access token beside the configuration file.
func linearTokenFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "linear.json")
}

// mailConfigFile keeps the mailbox's IMAP credentials beside the configuration file.
func mailConfigFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "mail.json")
}

// reviewerSummary describes the reviewer_model for doctor.
func reviewerSummary(c config.Config) string {
	reviewer, model := c.Reviewer()
	if endpoint, ok := c.Endpoint(reviewer); ok {
		return fmt.Sprintf("Reviewer  %s %s · Decisions API at %s", safe(reviewer), safe(model), safe(endpoint.BaseURL))
	}
	if reviewer == "claude" {
		return fmt.Sprintf("Reviewer  Claude Code %s · reasoning %s", safe(model), provider.ClaudeReviewerReasoning)
	}
	if model == "" {
		model = provider.CodexReviewerModel
	}
	return fmt.Sprintf("Reviewer  Codex %s · reasoning %s · priority", safe(model), provider.CodexReviewerReasoning)
}

func doctor(ctx context.Context, c config.Config, path string, roles []agents.Role, dir string) error {
	fmt.Printf("JULIA %s\nConfig    %s\n", version, path)
	if names, err := config.Profiles(filepath.Dir(path)); err == nil {
		fmt.Printf("Profile   %s · profiles: %s\n", safe(profileLabel(path)), strings.Join(names, ", "))
	}
	fmt.Printf("Provider  %s\nMode      %s\n", c.Provider, c.Mode)
	model, reasoning := modelSettings(c)
	fmt.Printf("Model     %s\nReasoning %s\n", safe(model), safe(reasoning))
	for _, line := range contextSummary(c) {
		fmt.Println(line)
	}
	fmt.Println(reviewerSummary(c))
	for _, line := range rolesSummary(c, roles) {
		fmt.Println(line)
	}
	keyFile := searchKeyFile(path)
	if os.Getenv(tools.WebSearchKeyEnv) != "" {
		fmt.Printf("Search    Firecrawl (%s)\n", tools.WebSearchKeyEnv)
	} else if key, err := tools.LoadSearchKey(keyFile); err != nil {
		fmt.Printf("Search    Firecrawl key unreadable: %s\n", safe(err.Error()))
	} else if key != "" {
		fmt.Printf("Search    Firecrawl (%s)\n", keyFile)
	} else {
		fmt.Println("Search    Firecrawl, set up on first WebSearch")
	}
	if os.Getenv(tools.LinearTokenEnv) != "" {
		fmt.Printf("Linear    app token (%s)\n", tools.LinearTokenEnv)
	} else if token, err := tools.LoadLinearToken(linearTokenFile(path)); err != nil {
		fmt.Printf("Linear    token unreadable: %s\n", safe(err.Error()))
	} else if token != "" {
		fmt.Printf("Linear    app token (%s)\n", linearTokenFile(path))
	} else {
		fmt.Printf("Linear    off; save a token in %s\n", linearTokenFile(path))
	}
	if mailConfig, err := tools.LoadMailConfig(mailConfigFile(path)); err != nil {
		fmt.Printf("Mail      config unreadable: %s\n", safe(err.Error()))
	} else if mailConfig.Enabled() {
		fmt.Printf("Mail      %s (%s)\n", safe(mailConfig.Summary()), mailConfigFile(path))
	} else {
		fmt.Printf("Mail      off; save IMAP credentials in %s\n", mailConfigFile(path))
	}
	for _, line := range memorySummary(c, dir) {
		fmt.Println(line)
	}
	for _, name := range []string{"rg", "bash"} {
		p, err := exec.LookPath(name)
		if err != nil {
			fmt.Printf("%-10s missing\n", name)
		} else {
			fmt.Printf("%-10s %s\n", name, p)
		}
	}
	b, err := findCodex(c.CodexBinary)
	if err == nil {
		fmt.Println("Codex    ", b)
		cmd := exec.CommandContext(ctx, b, "login", "status")
		out, loginErr := cmd.CombinedOutput()
		fmt.Println("Login    ", strings.TrimSpace(safe(string(out))))
		if loginErr != nil {
			fmt.Println("Run julia login to sign in.")
		}
	} else {
		fmt.Println("Codex     unavailable")
	}
	if b, err := findClaude(c.ClaudeBinary); err == nil {
		fmt.Println("Claude   ", b)
		fmt.Println("Login    ", claudeLoginStatus(ctx, b))
	} else {
		fmt.Println("Claude    unavailable")
	}
	bundle, skillErr := instructions.Build(instructions.Options{SkillPaths: c.SkillPaths, CodexHome: os.Getenv("CODEX_HOME")})
	if skillErr != nil {
		return fmt.Errorf("check skills: %w", skillErr)
	}
	for _, warning := range bundle.Warnings {
		fmt.Println("Skills    ", safe(warning))
	}
	for _, path := range bundle.Skills {
		fmt.Println("Skill     ", safe(path))
	}
	fmt.Println(browserSummary(ctx, c.BrowserPath()))
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probeDir, err := os.MkdirTemp("", "julia-doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeDir)
	// Yolo mode has no sandbox to probe; the probe then checks that bash
	// itself runs. Every other mode's Bash goes through the sandbox.
	probeMode := "auto"
	if c.Mode == "yolo" {
		probeMode = "yolo"
	}
	probe, err := tools.New(tools.Config{Directory: probeDir, Mode: probeMode})
	if err != nil {
		return err
	}
	defer probe.Close()
	fmt.Println("Sandbox  ", probe.SandboxInfo())
	if err := probe.CheckSandbox(checkCtx); err != nil {
		return fmt.Errorf("sandbox probe failed (Bash will remain blocked): %w", err)
	}
	fmt.Println("Sandbox probe passed.")
	return nil
}

// browserSummary is the doctor's line for the Browser tool: the Chrome it
// would launch and its version, once started and closed again, or why there
// is none and how to get one. A browser that fails is no failure of the
// doctor: only the Browser tool needs it.
func browserSummary(ctx context.Context, explicit string) string {
	binary, err := browser.Find(explicit)
	if err != nil {
		return "Browser   " + safe(err.Error())
	}
	launchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := browser.Launch(launchCtx, browser.Options{Binary: binary})
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		return "Browser   " + safe(binary) + " · failed to start: " + safe(first)
	}
	defer b.Close()
	return "Browser   " + safe(binary) + " · " + safe(b.Version())
}

// contextSummary is the doctor's view of the context budget: the configured
// model's window, which is its own context_window when it has one. Only API
// providers are summarized by JULIA, so only they have a compaction point;
// Codex compacts its own threads. A leftover context_bytes is called out
// because it silently stopped doing anything.
func contextSummary(c config.Config) []string {
	window := c.ContextWindowFor(c.Provider, c.Model)
	line := "Context   " + core.CompactTokens(window) + " tokens"
	if window != c.ContextTokens {
		line += " · the model's own context_window"
	}
	if c.Provider == "codex" {
		line += " · Codex compacts its own threads"
	} else if c.Provider == "claude" {
		line += " · Claude Code compacts its own sessions"
	} else {
		line += " · compacts at " + core.CompactTokens(core.CompactionThreshold(window))
	}
	lines := []string{line}
	if c.ContextBytes != 0 {
		lines = append(lines, fmt.Sprintf("Context   context_bytes (%d) is deprecated and ignored; context_tokens (%d) is used instead", c.ContextBytes, c.ContextTokens))
	}
	return lines
}

// tokenSummary is the headless one-line session total. In is the whole prompt,
// cached or not; the parenthesis is the share the provider served from its cache.
func tokenSummary(u core.TokenUsage) string {
	in := core.CompactTokens(u.Prompt()) + " in"
	if u.CacheRead > 0 {
		in += " (" + core.CompactTokens(u.CacheRead) + " cached)"
	}
	return in + " · " + core.CompactTokens(u.Output) + " out"
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		if r == 127 {
			return -1
		}
		return r
	}, ansi.Strip(s))
}
