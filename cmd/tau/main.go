package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/auth/oauth"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/internal/tui"
	"github.com/ihavespoons/tau/session"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tau: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-v", "version":
			fmt.Printf("tau %s (%s, %s)\n", version, commit, date)
			return nil
		case "login":
			return login(args[1:])
		case "logout":
			return logout(args[1:])
		case "models":
			return listModels()
		case "sessions":
			return listSessions()
		}
	}
	return printMode(args)
}

func store() auth.CredentialStore { return auth.NewFileStore(config.AuthPath()) }

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// printMode is the non-interactive `tau -p "prompt"` path: run the agent
// loop with tools and stream its work to stdout.
func printMode(args []string) error {
	fs := flag.NewFlagSet("tau", flag.ContinueOnError)
	var (
		modelID   = fs.String("model", "", "model id")
		thinking  = fs.String("thinking", "", "thinking level: off|minimal|low|medium|high|xhigh|max")
		system    = fs.String("system-prompt", "", "system prompt override")
		print     = fs.Bool("print", false, "print mode (non-interactive)")
		noTools   = fs.Bool("no-tools", false, "disable tools")
		noSession = fs.Bool("no-session", false, "do not persist a session")
		cont      = fs.Bool("continue", false, "continue the most recent session for this directory")
		sessPath  = fs.String("session", "", "resume a specific session file")
		verbose   = fs.Bool("verbose", false, "show tool calls and usage")
		mode      = fs.String("mode", "text", "output mode: text|json")
		approve   = fs.Bool("approve", false, "trust this project's .tau resources")
		noApprove = fs.Bool("no-approve", false, "do not trust this project's .tau resources")
	)
	fs.BoolVar(print, "p", false, "print mode (non-interactive)")
	fs.BoolVar(cont, "c", false, "continue the most recent session")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `tau — a coding agent for your terminal

usage:
  tau                    start an interactive session
  tau -c                 continue the most recent session here
  tau -p "prompt"        run the agent (non-interactive)
  tau -p -c "prompt"     continue the most recent session here
  tau login [provider]   log in (default: anthropic; also github-copilot, openai-codex)
  tau login -k           store an API key instead
  tau logout [provider]  remove stored credentials
  tau models             list available models
  tau sessions           list sessions for this directory
  tau --version

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	piped := false
	if prompt == "" {
		stat, _ := os.Stdin.Stat()
		if stat != nil && stat.Mode()&os.ModeCharDevice == 0 {
			piped = true
			b, err := os.ReadFile("/dev/stdin")
			if err == nil {
				prompt = strings.TrimSpace(string(b))
			}
		}
	}

	ctx, cancel := ctxWithSignals()
	defer cancel()

	var trustOverride *bool
	if *approve {
		yes := true
		trustOverride = &yes
	} else if *noApprove {
		no := false
		trustOverride = &no
	}

	// No prompt and a real terminal means interactive: that is the default way
	// to run tau, and every other mode is an explicit opt-in.
	if prompt == "" {
		if piped || *print {
			fs.Usage()
			return errors.New("no prompt given")
		}
		return tui.Run(ctx, tui.Options{Coding: coding.Options{
			ModelID:       *modelID,
			ThinkingLevel: ai.ModelThinkingLevel(*thinking),
			SystemPrompt:  *system,
			NoTools:       *noTools,
			NoSession:     *noSession,
			Resume:        *cont,
			SessionPath:   *sessPath,
			TrustOverride: trustOverride,
			Extensions:    bundledExtensions(),
		}})
	}

	extMode := extension.ModePrint
	if *mode == "json" {
		extMode = extension.ModeJSON
	}

	cs, err := coding.New(ctx, coding.Options{
		ModelID:       *modelID,
		ThinkingLevel: ai.ModelThinkingLevel(*thinking),
		SystemPrompt:  *system,
		NoTools:       *noTools,
		NoSession:     *noSession,
		Resume:        *cont,
		SessionPath:   *sessPath,
		Mode:          extMode,
		TrustOverride: trustOverride,
		Extensions:    bundledExtensions(),
	})
	if err != nil {
		return err
	}
	defer cs.Close(ctx, "exit")

	if *mode == "json" {
		return runJSONMode(ctx, cs, prompt)
	}
	if *verbose {
		fmt.Fprintln(os.Stderr, "tau: "+cs.Describe())
		if !cs.Trust.Trusted {
			fmt.Fprintln(os.Stderr, "tau: project resources not loaded ("+cs.Trust.Reason+")")
		}
	}

	out := &flushWriter{w: os.Stdout}
	inThinking := false
	cs.Agent.Subscribe(func(_ context.Context, ev agent.Event) error {
		switch ev.Type {
		case agent.EventMessageUpdate:
			if ev.StreamEvent == nil {
				return nil
			}
			switch ev.StreamEvent.Type {
			case ai.EventThinkingStart:
				inThinking = true
				out.write("\x1b[2m") // dim thinking
			case ai.EventThinkingDelta:
				out.write(ev.StreamEvent.Delta)
			case ai.EventThinkingEnd:
				inThinking = false
				out.write("\x1b[0m\n")
			case ai.EventTextDelta:
				out.write(ev.StreamEvent.Delta)
			}
		case agent.EventToolExecutionStart:
			out.write(fmt.Sprintf("\n\x1b[36m· %s\x1b[0m %s\n", ev.ToolName, summarizeArgs(ev.Args)))
		case agent.EventToolExecutionEnd:
			if ev.IsError {
				out.write(fmt.Sprintf("\x1b[31m  ↳ error: %s\x1b[0m\n", firstText(ev.Result)))
			} else if *verbose {
				out.write(fmt.Sprintf("\x1b[2m  ↳ %s\x1b[0m\n", truncateLine(firstText(ev.Result), 100)))
			}
		}
		return nil
	})

	if _, err := cs.Prompt(ctx, prompt); err != nil {
		return err
	}
	if inThinking {
		out.write("\x1b[0m")
	}
	out.write("\n")
	if out.err != nil {
		return out.err
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "tau: %s\n", coding.FormatUsage(cs.Usage()))
	}
	if msg := cs.Agent.ErrorMessage(); msg != "" {
		return errors.New(msg)
	}
	return nil
}

// summarizeArgs renders tool arguments compactly for the activity line.
func summarizeArgs(args map[string]any) string {
	for _, key := range []string{"path", "command", "pattern"} {
		if v, ok := args[key]; ok {
			return truncateLine(fmt.Sprint(v), 80)
		}
	}
	return ""
}

func firstText(r *agent.ToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if t, ok := r.Content[0].(ai.TextContent); ok {
		return t.Text
	}
	return ""
}

func truncateLine(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// listSessions prints the sessions recorded for this directory.
func listSessions() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repo := session.NewJSONLRepo(config.SessionsDir())
	metas, err := repo.List(context.Background(), cwd)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		fmt.Println("no sessions for " + cwd)
		return nil
	}
	for _, m := range metas {
		fmt.Printf("%s  %s\n", m.CreatedAt, m.Path)
	}
	return nil
}

// flushWriter writes streamed deltas straight through, remembering the first
// error so the caller reports it once rather than at every delta.
type flushWriter struct {
	w   *os.File
	err error
}

func (f *flushWriter) write(s string) {
	if f.err != nil || s == "" {
		return
	}
	if _, err := f.w.WriteString(s); err != nil {
		f.err = err
	}
}

// listModels prints everything the registry knows, built-in and configured
// alike. It has to go through the registry rather than a provider: a model
// declared in models.json is only reachable by its qualified
// "provider/id" spec, and printing the bare id would name something the user
// then cannot select.
func listModels() error {
	reg, warnings, err := coding.BuildRegistry(store())
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}

	for _, p := range reg.Providers() {
		fmt.Printf("\n%s\n", p.Name)
		for _, m := range p.Models {
			levels := ai.SupportedThinkingLevels(&m)
			fmt.Printf("  %-34s %-30s ctx %8d  out %6d  $%.2f/$%.2f per Mtok  thinking: %v\n",
				p.ID+"/"+m.ID, m.Name, m.ContextWindow, m.MaxTokens,
				m.Cost.Input, m.Cost.Output, levels)
		}
	}
	return nil
}

func logout(args []string) error {
	provider := "anthropic"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		provider = args[0]
	}

	ctx, cancel := ctxWithSignals()
	defer cancel()
	if err := store().Delete(ctx, provider); err != nil {
		return err
	}
	fmt.Printf("Removed stored %s credentials.\n", provider)
	return nil
}

// cliInteraction implements auth.Interaction against the terminal.
type cliInteraction struct{ in *bufio.Reader }

func (c cliInteraction) Prompt(_ context.Context, p auth.Prompt) (string, error) {
	if p.Message != "" {
		fmt.Fprintln(os.Stderr, p.Message)
	}
	if p.Placeholder != "" {
		fmt.Fprintf(os.Stderr, "(%s)\n", p.Placeholder)
	}
	fmt.Fprint(os.Stderr, "> ")
	line, err := c.in.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (c cliInteraction) Notify(ev auth.Event) {
	switch ev.Type {
	case auth.EventAuthURL:
		if ev.Message != "" {
			fmt.Fprintln(os.Stderr, ev.Message)
		}
		fmt.Fprintf(os.Stderr, "\nOpen this URL to authorize tau:\n\n  %s\n\n", ev.URL)
		if ev.Instructions != "" {
			fmt.Fprintln(os.Stderr, ev.Instructions)
		}
	case auth.EventDeviceCode:
		fmt.Fprintf(os.Stderr, "Enter code %s at %s\n", ev.UserCode, ev.URL)
	default:
		if ev.Message != "" {
			fmt.Fprintln(os.Stderr, ev.Message)
		}
		for _, l := range ev.Links {
			fmt.Fprintf(os.Stderr, "  %s %s\n", l.Label, l.URL)
		}
	}
}

// loginFlows are the providers tau can log in to interactively. Everything
// else authenticates with an API key from the environment, which needs no
// command.
var loginFlows = map[string]func() auth.OAuthAuth{
	"anthropic":      func() auth.OAuthAuth { return oauth.NewAnthropic() },
	"github-copilot": func() auth.OAuthAuth { return oauth.NewCopilot() },
	"openai-codex":   func() auth.OAuthAuth { return oauth.NewCodex() },
}

func loginProviders() []string {
	ids := make([]string, 0, len(loginFlows))
	for id := range loginFlows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func login(args []string) error {
	useKey := false
	provider := "anthropic"
	for _, arg := range args {
		switch {
		case arg == "--api-key" || arg == "-k":
			useKey = true
		case !strings.HasPrefix(arg, "-"):
			provider = arg
		}
	}

	ctx, cancel := ctxWithSignals()
	defer cancel()

	in := cliInteraction{in: bufio.NewReader(os.Stdin)}
	s := store()

	if useKey {
		key, err := in.Prompt(ctx, auth.Prompt{
			Type: auth.PromptSecret, Message: "Paste your " + provider + " API key:",
		})
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("no key entered")
		}
		if _, err := s.Modify(ctx, provider, func(*auth.Credential) (*auth.Credential, error) {
			return &auth.Credential{Type: auth.CredentialAPIKey, Key: key}, nil
		}); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Saved API key to "+config.AuthPath())
		return nil
	}

	flow, ok := loginFlows[provider]
	if !ok {
		return fmt.Errorf("no login flow for %q — tau can log in to: %s\n"+
			"every other provider takes an API key from the environment",
			provider, strings.Join(loginProviders(), ", "))
	}
	if err := oauth.Login(ctx, flow(), s, provider, in); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Logged in. Credentials saved to "+config.AuthPath())
	return nil
}

// runJSONMode streams the run as JSONL on stdout, one event per line, so tau
// is scriptable in CI the way `pi --mode json` is.
func runJSONMode(ctx context.Context, cs *coding.Session, prompt string) error {
	jw := coding.NewJSONWriter(os.Stdout)
	jw.Emit(coding.JSONEvent{
		Type: "session_start", SessionPath: cs.Path, Model: cs.Model.ID,
	})
	cs.Agent.Subscribe(jw.Sink())

	_, promptErr := cs.Prompt(ctx, prompt)

	usage := cs.Usage()
	final := coding.JSONEvent{Type: "result", Usage: &usage}
	if promptErr != nil {
		final.Error = promptErr.Error()
	} else if msg := cs.Agent.ErrorMessage(); msg != "" {
		final.Error = msg
	}
	jw.Emit(final)

	if err := jw.Err(); err != nil {
		return err
	}
	if final.Error != "" {
		return errors.New(final.Error)
	}
	return nil
}
