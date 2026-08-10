package main

import (
	"context"
	"os"

	"github.com/ihavespoons/tau/acp"
	"github.com/ihavespoons/tau/coding"
)

// runACPMode serves the Agent Client Protocol on stdin and stdout, so an editor
// that drives ACP agents can drive tau.
//
// Unlike every other mode there is no session up front: the client says which
// working directory it wants with session/new, and may open several. The
// options here are the template each one is built from.
//
// Nothing may reach stdout that is not an ACP message — the transport says so —
// so anything tau wants to say goes to stderr.
func runACPMode(ctx context.Context, template coding.Options) error {
	agent := acp.NewAgent(func(ctx context.Context, cwd string) (*coding.Session, error) {
		opts := template
		opts.Cwd = cwd
		return coding.New(ctx, opts)
	}, version)

	return agent.Serve(ctx, os.Stdin, os.Stdout)
}
