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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"jarvis/agents"
	"jarvis/internal/browser"
	"jarvis/internal/config"
	"jarvis/internal/core"
	"jarvis/internal/instructions"
	"jarvis/internal/provider"
	"jarvis/internal/session"
	"jarvis/internal/tools"
	"jarvis/internal/ui"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jarvis:", safe(err.Error()))
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
	flags := flag.NewFlagSet("jarvis", flag.ContinueOnError)
	var path, dir, prompt, modelFlag, reasoning, mode, endpoint, keyEnv, resume, timeout string
	var cont, demo, jsonMode, showVersion bool
	var snapshot int
	flags.StringVar(&path, "config", configPath, "configuration file")
	flags.StringVar(&dir, "dir", ".", "workspace directory")
	flags.StringVar(&prompt, "prompt", "", "run one task without the TUI")
	flags.StringVar(&modelFlag, "model", "", "provider/<model-id>: codex, openai, anthropic, google or compatible (bare codex uses your Codex model)")
	flags.StringVar(&reasoning, "reasoning", "", "Reasoning effort (Codex model-supported value or OpenRouter value; model support varies)")
	flags.StringVar(&mode, "mode", "", "auto, ask, or read-only")
	flags.StringVar(&endpoint, "compatible-base-url", "", "compatible provider API endpoint")
	flags.StringVar(&keyEnv, "api-key-env", "", "environment variable containing API key")
	flags.StringVar(&resume, "resume", "", "resume a saved session ID")
	flags.StringVar(&timeout, "timeout", "", "maximum duration per task, e.g. 4h")
	flags.BoolVar(&cont, "continue", false, "resume latest session in this directory")
	flags.BoolVar(&demo, "demo", false, "explore the interface offline")
	flags.BoolVar(&jsonMode, "json", false, "emit JSON events in headless mode")
	flags.BoolVar(&showVersion, "version", false, "print version")
	flags.IntVar(&snapshot, "snapshot", 0, "render an offline demo at this terminal width")
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), "JARVIS · a quiet interface for ambitious work\n\nUsage: jarvis [flags]\n       jarvis doctor | login | sessions | config init\n")
		flags.PrintDefaults()
	}
	if err = flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if showVersion {
		fmt.Println("jarvis", version)
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
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
	if mode != "" {
		cfg.Mode = mode
	}
	if endpoint != "" {
		cfg.CompatibleBaseURL = endpoint
	}
	if keyEnv != "" {
		cfg.APIKeyEnv = keyEnv
	}
	if timeout != "" {
		cfg.Timeout = timeout
	}
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
	if len(args) > 0 {
		switch args[0] {
		case "doctor":
			return doctor(ctx, cfg, path, roles)
		case "login":
			cmd := exec.CommandContext(ctx, cfg.CodexBinary, "login")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		case "config":
			if len(args) != 2 || args[1] != "init" {
				return errors.New("usage: jarvis config init")
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
	demoTools := append(tools.Definitions(roles), tools.QuestionDefinition())
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
		preview.Demo = true
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
	ask, questions := headlessAsker, (<-chan ui.QuestionRequest)(nil)
	if prompt == "" {
		ask, questions = ui.NewAsker()
	}
	duration, _ := time.ParseDuration(cfg.Timeout)
	runner := &runtimeBackend{backend: raw, cfg: cfg, roles: roles, approve: approve, review: review, ask: ask, makeBackend: factory, timeout: duration, parent: ctx, headless: prompt != "", checkpoint: store.Save, searchKeyFile: searchKeyFile(path)}
	if prompt != "" {
		s.Title = session.Title(prompt)
		enc := json.NewEncoder(os.Stdout)
		emit := func(e core.Event) {
			if jsonMode {
				_ = enc.Encode(e)
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
	uiConfig.Hangup = hangup
	uiConfig.ApplySettings = func(current *core.Session, selectedProvider, selectedModel, reasoning string) error {
		if selectedProvider == "compatible" && cfg.Enables("compatible") {
			if cfg.CompatibleBaseURL == "" {
				return errors.New("configure compatible_base_url for the compatible provider")
			}
			keyEnv := compatibleAPIKeyEnv(cfg)
			if os.Getenv(keyEnv) == "" {
				return fmt.Errorf("set %s before switching to the compatible provider", keyEnv)
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
	for _, entry := range cfg.EnabledModels {
		provider, id := config.SplitModel(entry.ID)
		uiConfig.EnabledModels = append(uiConfig.EnabledModels, core.ModelOption{Provider: provider, ID: id, Name: id, Description: "Enabled model", DefaultReasoning: entry.Reasoning})
	}
	uiConfig.CompatibleReasoning = config.SupportsReasoning("compatible", cfg.CompatibleBaseURL)
	codexBinary := cfg.CodexBinary
	if codex, ok := raw.(*provider.Codex); ok {
		codexBinary = codex.Binary
	}
	uiConfig.Models = func(ctx context.Context) ([]core.ModelOption, error) {
		binary, err := findCodex(codexBinary)
		if err != nil {
			return nil, err
		}
		return (&provider.Codex{Binary: binary}).Models(ctx)
	}
	uiConfig.WeeklyUsage = func(ctx context.Context) (*core.WeeklyUsage, error) {
		binary, err := findCodex(codexBinary)
		if err != nil {
			return nil, err
		}
		return (&provider.Codex{Binary: binary}).WeeklyUsage(ctx)
	}
	return ui.Run(uiConfig)
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
			if s.Provider != "codex" {
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
		if cfg.Provider == "codex" {
			model = "inherited from Codex"
		}
	}
	if reasoning == "" {
		reasoning = "not configurable for this provider"
		if cfg.Provider == "codex" {
			reasoning = "inherited from Codex"
		} else if config.SupportsReasoning(cfg.Provider, cfg.CompatibleBaseURL) {
			reasoning = "provider/model default"
		}
	}
	return model, reasoning
}

type runtimeBackend struct {
	headless        bool
	checkpoint      func(*core.Session) error
	backend         core.Backend
	makeBackend     func(context.Context, config.Config) (core.Backend, core.Approver, error)
	cfg             config.Config
	approve, review core.Approver
	ask             core.Asker
	timeout         time.Duration
	parent          context.Context
	// searchKeyFile holds the Firecrawl key saved by WebSearch's first-use setup.
	searchKeyFile string
	// roles are the subagent roles a delegated task may name; children are run
	// by makeBackend, so without it there are none.
	roles []agents.Role
}

// headlessAsker answers for a run with no human: it skips the question at once.
func headlessAsker(context.Context, core.Question) (core.Answer, error) {
	return core.Answer{Skipped: true}, nil
}

func (r *runtimeBackend) Definitions() []core.ToolDefinition {
	definitions := tools.Definitions(r.subagentRoles())
	if r.ask != nil {
		definitions = append(definitions, tools.QuestionDefinition())
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
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	stop := context.AfterFunc(r.parent, cancel)
	defer stop()
	selected := r.cfg
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
	emit = func(event core.Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		if event.Kind == "tool_start" || event.Kind == "tool_end" || event.Kind == "warning" || event.Kind == "status" {
			s.Audit = append(s.Audit, core.AuditEntry{At: time.Now(), Event: event})
		}
		consumer(event)
	}
	task := newTaskContext(s.Messages, prompt.Text)
	executor, err := tools.New(tools.Config{Directory: s.Directory, Mode: r.cfg.Mode, Approve: r.approve, Review: guard, Task: task.String(), Emit: emit, SearchKey: os.Getenv(tools.WebSearchKeyEnv), SearchKeyFile: r.searchKeyFile, ScratchDir: filepath.Join(os.TempDir(), "jarvis-scratch", s.ID), Browser: r.cfg.BrowserPath()})
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
	bundle, err := instructions.Build(instructions.Options{
		Directory: s.Directory, Provider: s.Provider, Model: s.Model, Reasoning: selected.Reasoning, Mode: r.cfg.Mode,
		Sandbox: executor.SandboxInfo(), Timeout: r.timeout.String(), Headless: r.headless,
		SkillPaths: r.cfg.SkillPaths, CodexHome: os.Getenv("CODEX_HOME"),
		BashEnvironment: bashEnvironment,
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
	if s.Title == "New conversation" || s.Title == "" {
		s.Title = session.Title(prompt.Text)
	}
	var agentTools core.ToolExecutor = executor
	if roles := r.subagentRoles(); len(roles) > 0 {
		// Snapshot trusted settings before the parent provider changes its session.
		parentID, directory := s.ID, s.Directory
		agentTools = tools.WithSubagents(executor, roles, func(childCtx context.Context, role agents.Role, task string, childTools core.ToolExecutor) (string, error) {
			settings := subagentSettings(r.cfg, selected, role.Name)
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
			child.Title = "Subagent " + role.Name + ": " + session.Title(task)
			child.Reasoning, child.ReasoningSet = settings.Reasoning, true
			child.Instructions = instructionText + subagentInstructions(role)
			if r.checkpoint != nil {
				if err := r.checkpoint(child); err != nil {
					return "", err
				}
			}
			emit(core.Event{Kind: "status", Text: "Started " + role.Name + " on " + modelID + " · subagent " + child.ID})
			childEmit := func(event core.Event) {
				if event.Kind == "tool_start" || event.Kind == "tool_end" || event.Kind == "warning" || event.Kind == "status" {
					child.Audit = append(child.Audit, core.AuditEntry{At: time.Now(), Event: event})
				}
			}
			err = childBackend.Run(childCtx, child, core.Prompt{Text: task}, childTools, childEmit)
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
	agentTools = tools.WithQuestions(agentTools, r.ask)
	if r.ask != nil {
		// The user's answers are their own words: later approvals this turn are judged with them in view.
		agentTools = withAnswerContext(agentTools, task, executor.SetTask)
	}
	return backend.Run(ctx, s, prompt, agentTools, emit)

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

// searchKeyFile keeps the Firecrawl key beside the configuration file it serves.
func searchKeyFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "firecrawl.json")
}

// reviewerSummary describes the reviewer_model for doctor.
func reviewerSummary(c config.Config) string {
	reviewer, model := c.Reviewer()
	if reviewer == "compatible" {
		return fmt.Sprintf("Reviewer  Compatible %s · Decisions API at %s", safe(model), safe(c.CompatibleBaseURL))
	}
	if model == "" {
		model = provider.CodexReviewerModel
	}
	return fmt.Sprintf("Reviewer  Codex %s · reasoning %s · priority", safe(model), provider.CodexReviewerReasoning)
}

func doctor(ctx context.Context, c config.Config, path string, roles []agents.Role) error {
	fmt.Printf("JARVIS %s\nConfig    %s\nProvider  %s\nMode      %s\n", version, path, c.Provider, c.Mode)
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
			fmt.Println("Run jarvis login to sign in.")
		}
	} else {
		fmt.Println("Codex     unavailable")
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
	fmt.Println("Sandbox  ", tools.SandboxInfo())
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probeDir, err := os.MkdirTemp("", "jarvis-doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeDir)
	probe, err := tools.New(tools.Config{Directory: probeDir, Mode: "auto"})
	if err != nil {
		return err
	}
	defer probe.Close()
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

// contextSummary is the doctor's view of the context budget. Only API providers
// are summarized by JARVIS, so only they have a compaction point; Codex compacts
// its own threads. A leftover context_bytes is called out because it silently
// stopped doing anything.
func contextSummary(c config.Config) []string {
	line := "Context   " + core.CompactTokens(c.ContextTokens) + " tokens"
	if c.Provider == "codex" {
		line += " · Codex compacts its own threads"
	} else {
		line += " · compacts at " + core.CompactTokens(core.CompactionThreshold(c.ContextTokens))
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
