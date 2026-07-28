package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/auth/oauth"
	"github.com/ihavespoons/tau/ai/provider"
	"github.com/ihavespoons/tau/config"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const defaultModel = "claude-sonnet-5"

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
			return logout()
		case "models":
			return listModels()
		}
	}
	return printMode(args)
}

func store() auth.CredentialStore { return auth.NewFileStore(config.AuthPath()) }

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// printMode is the non-interactive `tau -p "prompt"` path: stream one
// assistant turn to stdout. Sessions, tools, and the agent loop land in P2.
func printMode(args []string) error {
	fs := flag.NewFlagSet("tau", flag.ContinueOnError)
	var (
		modelID  = fs.String("model", defaultModel, "model id")
		thinking = fs.String("thinking", "", "thinking level: off|minimal|low|medium|high|xhigh|max")
		system   = fs.String("system-prompt", "", "system prompt")
		maxTok   = fs.Int("max-tokens", 0, "max output tokens (0 = model default)")
		print    = fs.Bool("print", false, "print mode (non-interactive)")
	)
	fs.BoolVar(print, "p", false, "print mode (non-interactive)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `tau — a coding agent for your terminal

usage:
  tau -p "prompt"        stream one response (non-interactive)
  tau login              authenticate with Anthropic (Claude Pro/Max OAuth or API key)
  tau logout             remove stored credentials
  tau models             list available models
  tau --version

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		stat, _ := os.Stdin.Stat()
		if stat != nil && stat.Mode()&os.ModeCharDevice == 0 {
			b, err := os.ReadFile("/dev/stdin")
			if err == nil {
				prompt = strings.TrimSpace(string(b))
			}
		}
	}
	if prompt == "" {
		fs.Usage()
		return errors.New("no prompt given (interactive mode lands in P4)")
	}

	p := provider.Anthropic(store(), auth.OSContext{})
	model := p.Model(*modelID)
	if model == nil {
		return fmt.Errorf("unknown model %q (try `tau models`)", *modelID)
	}

	opts := &ai.SimpleStreamOptions{}
	if *maxTok > 0 {
		opts.MaxTokens = *maxTok
	}
	if *thinking != "" && *thinking != "off" {
		clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(*thinking))
		if clamped != ai.ThinkingOff {
			opts.Reasoning = ai.ThinkingLevel(clamped)
		}
	}

	c := ai.Context{
		SystemPrompt: *system,
		Messages: ai.MessageList{ai.UserMessage{
			Content: ai.UserContent{Text: prompt}, Timestamp: time.Now().UnixMilli(),
		}},
	}

	ctx, cancel := ctxWithSignals()
	defer cancel()

	stream := p.StreamSimple(ctx, model, c, opts)
	out := &flushWriter{w: os.Stdout}
	inThinking := false
	for ev := range stream.Events() {
		switch ev.Type {
		case ai.EventThinkingStart:
			inThinking = true
			out.write("\x1b[2m") // dim thinking
		case ai.EventThinkingDelta:
			out.write(ev.Delta)
		case ai.EventThinkingEnd:
			inThinking = false
			out.write("\x1b[0m\n")
		case ai.EventTextDelta:
			out.write(ev.Delta)
		case ai.EventToolCallEnd:
			out.write(fmt.Sprintf("\n[tool: %s]\n", ev.ToolCall.Name))
		}
	}
	if inThinking {
		out.write("\x1b[0m")
	}
	out.write("\n")
	if out.err != nil {
		return out.err
	}

	final := stream.Result()
	if final != nil && final.StopReason == ai.StopError {
		return errors.New(final.ErrorMessage)
	}
	if final != nil && final.StopReason == ai.StopAborted {
		return errors.New("aborted")
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

func listModels() error {
	p := provider.Anthropic(store(), auth.OSContext{})
	for _, m := range p.Models {
		levels := ai.SupportedThinkingLevels(&m)
		fmt.Printf("%-20s %-22s ctx %8d  out %6d  $%.2f/$%.2f per Mtok  thinking: %v\n",
			m.ID, m.Name, m.ContextWindow, m.MaxTokens, m.Cost.Input, m.Cost.Output, levels)
	}
	return nil
}

func logout() error {
	ctx, cancel := ctxWithSignals()
	defer cancel()
	if err := store().Delete(ctx, "anthropic"); err != nil {
		return err
	}
	fmt.Println("Removed stored Anthropic credentials.")
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

func login(args []string) error {
	useKey := len(args) > 0 && (args[0] == "--api-key" || args[0] == "-k")
	ctx, cancel := ctxWithSignals()
	defer cancel()

	in := cliInteraction{in: bufio.NewReader(os.Stdin)}
	s := store()

	if useKey {
		key, err := in.Prompt(ctx, auth.Prompt{
			Type: auth.PromptSecret, Message: "Paste your Anthropic API key:",
		})
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("no key entered")
		}
		if _, err := s.Modify(ctx, "anthropic", func(*auth.Credential) (*auth.Credential, error) {
			return &auth.Credential{Type: auth.CredentialAPIKey, Key: key}, nil
		}); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Saved API key to "+config.AuthPath())
		return nil
	}

	if err := oauth.Login(ctx, oauth.NewAnthropic(), s, "anthropic", in); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Logged in. Credentials saved to "+config.AuthPath())
	return nil
}
