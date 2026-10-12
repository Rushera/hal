// Command hal is a minimal coding agent written in Go with bubbletea. It runs
// an agent loop over an OpenAI-compatible provider with the
// read/bash/edit/write/grep/find/ls tools, a terminal UI, JSONL sessions, and
// print/JSON modes.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/config"
	"github.com/Rushera/hal/internal/provider"
	"github.com/Rushera/hal/internal/session"
	"github.com/Rushera/hal/internal/tools"
	"github.com/Rushera/hal/internal/tui"
)

const version = "0.1.0"

type options struct {
	printMode    bool
	mode         string
	cont         bool
	sessionPath  string
	sessionDir   string
	noSession    bool
	name         string
	model        string
	providerName string
	apiKey       string
	baseURL      string
	toolList     string
	noContext    bool
	systemPrompt string
	appendPrompt string
	maxTurns     int
	configPath   string
	showConfig   bool
	showVersion  bool
	prompts      []string
	files        []string
}

func main() {
	if err := runWithArgs(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hal: "+err.Error())
		os.Exit(1)
	}
}

func run() error { return runWithArgs(os.Args[1:]) }

// runWithArgs is the whole program, parameterized by argv so tests can drive it.
func runWithArgs(args []string) error {
	opts, setFlags, err := parseArgs(args)
	if err != nil {
		return err
	}

	// Configuration file (XDG config dir) supplies defaults; flags and
	// environment variables override it.
	paths := config.Resolve()
	explicit := opts.configPath
	if explicit == "" {
		explicit = os.Getenv("HAL_CONFIG")
	}
	cfg, err := config.Load(explicit, paths)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "hal: warning: "+w)
	}
	opts, sources := resolveOptions(opts, setFlags, cfg)
	if err := validateResolved(opts); err != nil {
		return err
	}

	if opts.showConfig {
		printConfig(opts, sources, cfg, paths)
		return nil
	}
	if opts.showVersion {
		fmt.Println("hal " + version)
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// stdin piping: `cat file | hal -p "summarize"` merges stdin into the prompt
	var stdinText string
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		if data, err := io.ReadAll(io.LimitReader(os.Stdin, 4*1024*1024)); err == nil {
			stdinText = strings.TrimRight(string(data), "\n")
		}
	}

	prompt, err := buildPrompt(opts.prompts, opts.files, cwd, stdinText)
	if err != nil {
		return err
	}

	prov := provider.NewFromEnv(opts.apiKey, opts.baseURL, opts.model)
	// Identify ourselves, and carry a per-conversation id for gateways that
	// route on it (opencode.ai requires x-opencode-session).
	prov.UserAgent = "hal/" + version
	prov.SessionHeader = sessionHeaderFor(cfg.File.SessionHeader, prov.BaseURL)
	prov.SessionID = newRunID()
	toolList, err := tools.Select(cwd, opts.toolList)
	if err != nil {
		return err
	}

	sysPrompt, err := buildSystemPrompt(cwd, opts)
	if err != nil {
		return err
	}

	ag := agent.New(prov, toolList, cwd, "")
	ag.SystemPrompt = sysPrompt
	if opts.maxTurns > 0 {
		ag.MaxTurns = opts.maxTurns
	}

	// Sessions: JSONL files under the XDG data directory, unless --no-session.
	var sess *session.Session
	if !opts.noSession {
		root := session.RootWith(opts.sessionDir)
		switch {
		case opts.sessionPath != "":
			sess, err = session.Load(opts.sessionPath)
			if err != nil {
				return fmt.Errorf("cannot load session %s: %w", opts.sessionPath, err)
			}
			ag.History = append([]agent.Message(nil), sess.Messages...)
		case opts.cont:
			latest, lerr := session.LatestIn(session.SearchRoots(opts.sessionDir), cwd)
			if lerr != nil {
				return lerr
			}
			sess, err = session.Load(latest)
			if err != nil {
				return fmt.Errorf("cannot load session %s: %w", latest, err)
			}
			ag.History = append([]agent.Message(nil), sess.Messages...)
		default:
			sess, err = session.New(root, cwd, opts.name, prov.ModelName())
			if err != nil {
				return fmt.Errorf("cannot create session: %w", err)
			}
		}
		ag.OnMessage = func(m agent.Message) {
			if err := sess.AppendMessage(m); err != nil {
				fmt.Fprintln(os.Stderr, "hal: session write failed: "+err.Error())
			}
		}
		// The conversation id doubles as the provider's affinity id, so a
		// resumed session keeps the same routing bucket.
		if sess.ID != "" {
			ag.SetSessionID(sess.ID)
		}
		defer sess.Close()
	}

	jsonMode := strings.EqualFold(opts.mode, "json")
	interactive := !opts.printMode && !jsonMode &&
		isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
	if opts.mode != "" && strings.EqualFold(opts.mode, "text") && isatty.IsTerminal(os.Stdout.Fd()) {
		interactive = !opts.printMode
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case jsonMode:
		return runJSON(ctx, ag, prompt)
	case interactive:
		return runInteractive(ag, cwd, prompt, opts.sessionDir, sess)
	default:
		return runPrint(ctx, ag, prompt, os.Stdout, os.Stderr)
	}
}

// parseArgs parses the command line. The returned set records which flags the
// user actually typed, which is what makes flag-over-config precedence possible.
func parseArgs(args []string) (options, map[string]bool, error) {
	o := options{}
	fs := flag.NewFlagSet("hal", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText()) }

	fs.BoolVar(&o.printMode, "p", false, "run the prompt, print the response, exit")
	fs.BoolVar(&o.printMode, "print", false, "run the prompt, print the response, exit")
	fs.StringVar(&o.mode, "mode", "", "output mode: text or json")
	fs.BoolVar(&o.cont, "c", false, "continue the most recent session for this directory")
	fs.BoolVar(&o.cont, "continue", false, "continue the most recent session for this directory")
	fs.StringVar(&o.sessionPath, "session", "", "session file to open")
	fs.StringVar(&o.sessionDir, "session-dir", "", "session storage directory (default $XDG_DATA_HOME/hal/sessions)")
	fs.BoolVar(&o.noSession, "no-session", false, "do not save the session")
	fs.StringVar(&o.name, "n", "", "session display name")
	fs.StringVar(&o.name, "name", "", "session display name")
	fs.StringVar(&o.model, "model", "", "model id (default $HAL_MODEL, $OPENAI_MODEL, gpt-4o-mini)")
	fs.StringVar(&o.model, "m", "", "model id (shorthand for --model)")
	fs.StringVar(&o.providerName, "provider", "openai", "provider name (only openai-compatible is bundled)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key (default $HAL_API_KEY, $OPENAI_API_KEY)")
	fs.StringVar(&o.baseURL, "base-url", "", "endpoint base URL (default $HAL_BASE_URL, $OPENAI_BASE_URL)")
	fs.StringVar(&o.toolList, "t", "", "comma-separated tool allowlist (default: all built-ins)")
	fs.StringVar(&o.toolList, "tools", "", "comma-separated tool allowlist (default: all built-ins)")
	fs.BoolVar(&o.noContext, "nc", false, "do not load AGENTS.md/CLAUDE.md context files")
	fs.BoolVar(&o.noContext, "no-context-files", false, "do not load AGENTS.md/CLAUDE.md context files")
	fs.StringVar(&o.systemPrompt, "system-prompt", "", "replace the default system prompt")
	fs.StringVar(&o.appendPrompt, "append-system-prompt", "", "append text to the system prompt")
	fs.IntVar(&o.maxTurns, "max-turns", 32, "maximum agent turns per prompt")
	fs.StringVar(&o.configPath, "config", "", "configuration file (default $XDG_CONFIG_HOME/hal/config.toml)")
	fs.BoolVar(&o.showConfig, "show-config", false, "print the resolved configuration and exit")
	fs.BoolVar(&o.showVersion, "v", false, "print version and exit")
	fs.BoolVar(&o.showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return o, nil, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "@") && len(a) > 1 {
			o.files = append(o.files, strings.TrimPrefix(a, "@"))
			continue
		}
		o.prompts = append(o.prompts, a)
	}
	if o.mode != "" && !strings.EqualFold(o.mode, "text") && !strings.EqualFold(o.mode, "json") {
		return o, set, fmt.Errorf("unknown mode %q (want text or json)", o.mode)
	}
	return o, set, nil
}

// sessionHeaderFor decides which request header carries the conversation id.
// Gateways that document one get it automatically; other endpoints are left
// untouched unless the config file names a header explicitly.
func sessionHeaderFor(configured, baseURL string) string {
	if h := strings.TrimSpace(configured); h != "" {
		return h
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai") {
		return "x-opencode-session"
	}
	return ""
}

// newRunID generates a session affinity id for runs with no stored session.
func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// validateResolved rejects settings hal cannot honor, naming the config file
// when that is where the value came from.
func validateResolved(o options) error {
	if o.providerName != "" && !strings.EqualFold(o.providerName, "openai") {
		return fmt.Errorf("only the openai provider is bundled in hal (got %q); set provider = \"openai\"", o.providerName)
	}
	return nil
}

// settingSource records where a resolved value came from, for --show-config.
type settingSource struct {
	Key    string
	Value  string
	Source string
}

func anyFlagSet(set map[string]bool, names ...string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// resolveOptions applies the precedence flag > environment > config file >
// default to every configurable setting.
func resolveOptions(o options, set map[string]bool, cfg config.Loaded) (options, []settingSource) {
	var sources []settingSource

	pickString := func(key, flagVal string, flagNames, envNames []string, cfgVal, dflt string) string {
		if anyFlagSet(set, flagNames...) {
			sources = append(sources, settingSource{key, flagVal, "flag"})
			return flagVal
		}
		for _, e := range envNames {
			if v := strings.TrimSpace(os.Getenv(e)); v != "" {
				sources = append(sources, settingSource{key, v, "env $" + e})
				return v
			}
		}
		if strings.TrimSpace(cfgVal) != "" {
			sources = append(sources, settingSource{key, cfgVal, "config " + cfg.Path})
			return cfgVal
		}
		sources = append(sources, settingSource{key, dflt, "default"})
		return dflt
	}

	pickBool := func(key string, flagVal bool, flagNames []string, cfgVal *bool) bool {
		if anyFlagSet(set, flagNames...) {
			sources = append(sources, settingSource{key, boolText(flagVal), "flag"})
			return flagVal
		}
		if cfgVal != nil {
			sources = append(sources, settingSource{key, boolText(*cfgVal), "config " + cfg.Path})
			return *cfgVal
		}
		sources = append(sources, settingSource{key, boolText(flagVal), "default"})
		return flagVal
	}

	pickInt := func(key string, flagVal int, flagNames []string, cfgVal, dflt int) int {
		if anyFlagSet(set, flagNames...) {
			sources = append(sources, settingSource{key, fmt.Sprint(flagVal), "flag"})
			return flagVal
		}
		if cfgVal > 0 {
			sources = append(sources, settingSource{key, fmt.Sprint(cfgVal), "config " + cfg.Path})
			return cfgVal
		}
		sources = append(sources, settingSource{key, fmt.Sprint(dflt), "default"})
		return dflt
	}

	o.providerName = pickString("provider", o.providerName, []string{"provider"}, nil, cfg.File.Provider, "openai")
	o.model = pickString("model", o.model, []string{"model", "m"},
		[]string{"HAL_MODEL", "OPENAI_MODEL"}, cfg.File.Model, provider.DefaultModel)
	o.baseURL = pickString("base_url", o.baseURL, []string{"base-url"},
		[]string{"HAL_BASE_URL", "OPENAI_BASE_URL", "OPENAI_API_BASE"}, cfg.File.BaseURL, provider.DefaultBaseURL)
	// A config file may name the variable holding the key, so the secret can stay
	// in one place instead of being copied into a dotfile.
	keyEnvNames := make([]string, 0, 3)
	if name := strings.TrimSpace(cfg.File.APIKeyEnv); name != "" {
		keyEnvNames = append(keyEnvNames, name)
	}
	keyEnvNames = append(keyEnvNames, "HAL_API_KEY", "OPENAI_API_KEY")
	o.apiKey = pickString("api_key", o.apiKey, []string{"api-key"}, keyEnvNames, cfg.File.APIKey, "")
	o.toolList = pickString("tools", o.toolList, []string{"tools", "t"}, nil, cfg.File.Tools, "")
	o.maxTurns = pickInt("max_turns", o.maxTurns, []string{"max-turns"}, cfg.File.MaxTurns, 32)
	o.systemPrompt = pickString("system_prompt", o.systemPrompt, []string{"system-prompt"}, nil, cfg.File.SystemPrompt, "")
	o.appendPrompt = pickString("append_system_prompt", o.appendPrompt, []string{"append-system-prompt"}, nil, cfg.File.AppendSystemPrompt, "")
	o.sessionDir = pickString("session_dir", o.sessionDir, []string{"session-dir"},
		[]string{"HAL_SESSION_DIR"}, cfg.File.SessionDir, "")
	o.name = pickString("name", o.name, []string{"name", "n"}, nil, cfg.File.Name, "")
	o.noSession = pickBool("no_session", o.noSession, []string{"no-session"}, cfg.File.NoSession)
	o.noContext = pickBool("no_context_files", o.noContext, []string{"nc", "no-context-files"}, cfg.File.NoContextFiles)
	return o, sources
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// printConfig shows the resolved settings and where each value came from.
func printConfig(o options, sources []settingSource, cfg config.Loaded, paths config.Paths) {
	configFile := cfg.Path
	if configFile == "" {
		configFile = "(none found)"
	}
	fmt.Printf("hal %s\n\npaths:\n", version)
	fmt.Printf("  config file   %s\n", configFile)
	fmt.Printf("  config dir    %s\n", paths.ConfigDir)
	fmt.Printf("  session dir   %s\n", session.RootWith(o.sessionDir))
	fmt.Printf("  data dir      %s\n", paths.DataDir)
	fmt.Printf("  state dir     %s\n", paths.StateDir)
	fmt.Printf("\nsettings:\n")
	for _, s := range sources {
		fmt.Printf("  %-20s %s\n", s.Key, maskedValue(s.Key, s.Value))
		fmt.Printf("  %-20s   ← %s\n", "", s.Source)
	}
}

// maskedValue hides credentials in --show-config output.
func maskedValue(key, value string) string {
	if value == "" {
		return "(unset)"
	}
	lower := strings.ToLower(key)
	if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") {
		if len(value) <= 8 {
			return "***"
		}
		return value[:4] + "…" + value[len(value)-2:] + " (masked)"
	}
	return value
}

// buildPrompt merges messages, @file contents and piped stdin into one prompt.
func buildPrompt(prompts, files []string, cwd string, stdinText string) (string, error) {
	var parts []string
	if strings.TrimSpace(stdinText) != "" {
		parts = append(parts, stdinText)
	}
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read @%s: %w", f, err)
		}
		if len(data) > 512*1024 {
			data = data[:512*1024]
		}
		parts = append(parts, fmt.Sprintf("<file path=%q>\n%s\n</file>", f, strings.TrimRight(string(data), "\n")))
	}
	if text := strings.TrimSpace(strings.Join(prompts, " ")); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// buildSystemPrompt layers the default prompt, --system-prompt, context files
// and --append-system-prompt.
func buildSystemPrompt(cwd string, opts options) (string, error) {
	base := agent.BuildSystemPrompt(cwd, "")
	if strings.TrimSpace(opts.systemPrompt) != "" {
		if data, err := os.ReadFile(opts.systemPrompt); err == nil {
			base = string(data)
		} else {
			base = opts.systemPrompt
		}
	}
	var extras []string
	if !opts.noContext {
		files, err := contextFiles(cwd)
		if err != nil {
			return "", err
		}
		for _, f := range files {
			data, err := os.ReadFile(f.path)
			if err != nil {
				continue
			}
			extras = append(extras, fmt.Sprintf("Instructions from %s:\n%s", f.label, strings.TrimRight(string(data), "\n")))
		}
	}
	if strings.TrimSpace(opts.appendPrompt) != "" {
		text := opts.appendPrompt
		if data, err := os.ReadFile(text); err == nil {
			text = string(data)
		}
		extras = append(extras, strings.TrimRight(text, "\n"))
	}
	if len(extras) > 0 {
		base += "\n\n" + strings.Join(extras, "\n\n")
	}
	return base, nil
}

type contextFile struct {
	path  string
	label string
}

// contextFiles discovers AGENTS.md/CLAUDE.md from the global config directory,
// parent directories and the working directory, in that order.
func contextFiles(cwd string) ([]contextFile, error) {
	var out []contextFile
	paths := config.Resolve()
	seen := map[string]bool{}
	for _, g := range []struct{ dir, label string }{
		{paths.ConfigDir, "$XDG_CONFIG_HOME/hal"},
		{paths.LegacyDir, "~/.hal"},
	} {
		if g.dir == "" || seen[g.dir] {
			continue
		}
		seen[g.dir] = true
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(g.dir, name)
			if _, err := os.Stat(p); err == nil {
				out = append(out, contextFile{p, name + " in " + g.label})
				break // one context file per directory
			}
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return out, nil
	}
	var dirs []string
	for d := abs; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if parent := filepath.Dir(d); parent == d {
			break
		}
	}
	// outermost directory first, so the working directory wins
	for i := len(dirs) - 1; i >= 0; i-- {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(dirs[i], name)
			if _, err := os.Stat(p); err == nil {
				label := name + " in " + dirs[i]
				out = append(out, contextFile{p, label})
				break // one context file per directory
			}
		}
	}
	return out, nil
}

func runPrint(ctx context.Context, ag *agent.Agent, prompt string, out, errOut io.Writer) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("no prompt given; pass a message or use --print with stdin")
	}
	emitted := false
	err := ag.RunPrompt(ctx, prompt, func(ev agent.Event) {
		switch ev.Type {
		case agent.EventTextDelta:
			fmt.Fprint(out, ev.Text)
			emitted = true
		case agent.EventToolStart:
			if ev.ToolCall != nil {
				fmt.Fprintf(errOut, "⚙ %s(%s)\n", ev.ToolCall.Name, summarize(ev.ToolCall.Arguments))
			}
		case agent.EventToolResult:
			if ev.Result != nil {
				status := "ok"
				if ev.Result.IsError {
					status = "error"
				}
				fmt.Fprintf(errOut, "  ↳ %s (%dms)\n", status, ev.Result.DurationMS)
			}
		case agent.EventError:
			fmt.Fprintln(errOut, "hal: "+ev.Error)
		}
	})
	if emitted {
		fmt.Fprintln(out)
	}
	return err
}

func runJSON(ctx context.Context, ag *agent.Agent, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("no prompt given for --mode json")
	}
	enc := json.NewEncoder(os.Stdout)
	return ag.RunPrompt(ctx, prompt, func(ev agent.Event) {
		_ = enc.Encode(ev)
	})
}

func runInteractive(ag *agent.Agent, cwd, prompt, sessionDir string, sess *session.Session) error {
	opts := tui.Options{
		Agent:         ag,
		Cwd:           cwd,
		Version:       version,
		InitialPrompt: prompt,
	}
	if sess != nil {
		opts.SessionInfo = func() string {
			return fmt.Sprintf("id %s · %s · %d messages", sess.ID, sess.Path, len(sess.Messages))
		}
		opts.NewSession = func() (string, error) {
			ns, err := session.New(session.RootWith(sessionDir), cwd, "", ag.ModelName())
			if err != nil {
				return "", err
			}
			sess = ns
			ag.SetSessionID(ns.ID)
			ag.OnMessage = func(m agent.Message) { _ = ns.AppendMessage(m) }
			opts.SessionInfo = func() string {
				return fmt.Sprintf("id %s · %s · %d messages", ns.ID, ns.Path, len(ns.Messages))
			}
			return "(session " + ns.ID[:8] + ")", nil
		}
	}
	p := tea.NewProgram(tui.New(opts))
	_, err := p.Run()
	return err
}

func summarize(raw string) string {
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return truncate(strings.ReplaceAll(raw, "\n", " "), 60)
	}
	for _, key := range []string{"path", "command", "pattern"} {
		if v, ok := generic[key].(string); ok && v != "" {
			return truncate(strings.ReplaceAll(v, "\n", " "), 60)
		}
	}
	return truncate(strings.ReplaceAll(raw, "\n", " "), 60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func usageText() string {
	return `hal — a minimal coding agent in Go (bubbletea)

usage:
  hal [options] [--] [@files...] [messages...]

modes:
  (default)            interactive terminal UI
  -p, --print          run the prompt, print the response, exit
  --mode json          stream agent events as JSON lines

options:
  -c, --continue       continue the most recent session for this directory
  --session <path>     open a specific session file
  --no-session         do not persist the session
  -n, --name <name>    session display name
  --session-dir <dir>  session storage directory
  -m, --model <id>     model id
  --provider <name>    provider (only openai is bundled)
  --api-key <key>      API key
  --base-url <url>     OpenAI-compatible endpoint
  -t, --tools <list>   tool allowlist, e.g. "read,grep,find,ls"
  -nc, --no-context-files   skip AGENTS.md/CLAUDE.md
  --system-prompt <text|file>       replace the default system prompt
  --append-system-prompt <text|file> append to the system prompt
  --max-turns <n>      maximum agent turns per prompt (default 32)
  --config <path>      configuration file (default $XDG_CONFIG_HOME/hal/config.toml)
  --show-config        print the resolved configuration and exit
  -h, --help           show this help
  -v, --version        show the version

configuration file:
  TOML at $XDG_CONFIG_HOME/hal/config.toml (JSON also accepted as config.json).
  XDG_CONFIG_HOME defaults to ~/.config; a legacy ~/.hal/config.toml is read
  when the XDG file does not exist. Keys use snake_case and match the flag names:

    model    = "gpt-4o-mini"
    base_url = "https://api.openai.com/v1"
    api_key  = ""                 # prefer the HAL_API_KEY environment variable
    api_key_env = ""              # or name a variable, e.g. OPENCODE_GO_API_KEY
    tools    = "read,edit"        # comma-separated allowlist
    max_turns = 32
    system_prompt = ""            # text, or a path to a file
    no_context_files = false
    session_dir = ""              # default $XDG_DATA_HOME/hal/sessions
    session_header = ""           # "" = auto (x-opencode-session on opencode.ai)

  Precedence per setting: flag > environment > config file > default.
  Run 'hal --show-config' to see the resolved values and their sources.

environment:
  HAL_API_KEY / OPENAI_API_KEY      credentials
  HAL_BASE_URL / OPENAI_BASE_URL    endpoint (default https://api.openai.com/v1)
  HAL_MODEL / OPENAI_MODEL          model (default gpt-4o-mini)
  HAL_CONFIG                        configuration file path
  HAL_SESSION_DIR                   session storage root
  XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_STATE_HOME   base directories

examples:
  hal                                   start the interactive UI
  hal "list the go files in this repo"  interactive with an initial prompt
  hal -p "summarize README.md"          one-shot
  cat main.go | hal -p "review this"    pipe into a one-shot run
  hal -p @notes.md "extract the todos"  include a file
  hal --tools read,grep,find,ls -p "audit this repo"   read-only run
  hal -c                                resume the last session
`
}
