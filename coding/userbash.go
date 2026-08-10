package coding

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ihavespoons/tau/agent/env"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/session"
)

// BashPrefix marks a prompt as a shell command rather than a message to the
// model. Doubling it runs the command without telling the model about it.
const BashPrefix = "!"

// userBashMaxBytes caps captured output. The overflow goes to a file the
// message points at, so a command that prints a megabyte is still runnable
// without pushing the conversation out of the context window.
const userBashMaxBytes = 64 * 1024

// ErrNoCommand is returned when the prefix was typed with nothing after it.
var ErrNoCommand = errors.New("nothing to run")

// ParseUserBash reports whether a submitted line is a shell command, returning
// it with the prefix stripped.
//
// A doubled prefix keeps the result out of the model's context — for the
// command you are about to run twenty times while iterating, where each run
// would otherwise cost tokens and tell the model nothing new.
func ParseUserBash(text string) (command string, exclude, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimLeft(text, " \t"), BashPrefix)
	if !found {
		return "", false, false
	}
	if inner, doubled := strings.CutPrefix(rest, BashPrefix); doubled {
		return strings.TrimSpace(inner), true, true
	}
	return strings.TrimSpace(rest), false, true
}

// RunUserBash runs a command the user typed behind the prefix and records it.
//
// This is the user's own shell, not the model's tool: there is no approval
// path and no tool timeout, because the person typing it is the one who decided
// to run it. onOutput receives output as it arrives, so a slow command shows
// progress rather than nothing.
func (s *Session) RunUserBash(ctx context.Context, command string, exclude bool, onOutput func(string)) (*session.BashExecutionMessage, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, ErrNoCommand
	}

	msg := &session.BashExecutionMessage{
		Command:            command,
		ExcludeFromContext: exclude,
		Timestamp:          time.Now().UnixMilli(),
	}

	// An extension may answer instead of the shell — first non-nil wins — which
	// is how a sandbox or a remote executor takes over without the interface
	// knowing the difference.
	if s.Extensions != nil {
		if res := s.Extensions.EmitUserBash(ctx, &extension.UserBashEvent{Command: command}); res != nil {
			code := res.ExitCode
			msg.Output, msg.ExitCode = res.Output, &code
			if onOutput != nil && res.Output != "" {
				onOutput(res.Output)
			}
			return msg, s.recordUserBash(ctx, msg)
		}
	}

	res, err := s.Env.Exec(ctx, command, env.ExecOptions{
		MaxOutputBytes: userBashMaxBytes,
		OnOutput:       onOutput,
	})
	if err != nil {
		return nil, err
	}

	code := res.ExitCode
	msg.Output, msg.ExitCode = res.Output, &code
	msg.Cancelled = res.Cancelled
	msg.Truncated, msg.FullOutputPath = res.Truncated, res.FullOutputPath

	return msg, s.recordUserBash(ctx, msg)
}

// recordUserBash persists the execution and puts it in front of the model.
//
// The projection is the same one a restored session goes through, which is what
// makes the excluded case work: ConvertToLLM drops it, so the transcript keeps
// the command and the model never sees it.
func (s *Session) recordUserBash(ctx context.Context, msg *session.BashExecutionMessage) error {
	if s.Agent != nil {
		if converted := session.ConvertToLLM([]ai.Message{msg}); len(converted) > 0 {
			s.Agent.SetMessages(append(s.Agent.Messages(), converted...))
		}
	}
	if s.Session == nil {
		return nil
	}
	_, err := s.Session.AppendMessage(ctx, msg)
	return err
}
