package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ihavespoons/tau/server"
)

// serverCmd runs the supervisor: a long-lived process that owns several agents
// at once and hands them out over HTTP.
//
// This is the experimental satellite, not part of using tau as a coding agent.
// It exists for an editor plugin or a deployment that wants agents addressable
// by id and outliving any single connection, which `--mode rpc` cannot be
// because that is one agent on one pair of pipes.
func serverCmd(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", "", "address to listen on: a path is a Unix socket, anything else is TCP (default "+server.SocketPath()+")")
	if err := fs.Parse(args); err != nil {
		// Asking for help is not a failure, and reporting it as one puts a
		// spurious error line under the usage text the user asked to see.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	ln, err := server.Listen(*listen)
	if err != nil {
		return err
	}

	// Interrupt cancels the context, which takes the agents down with the
	// server: they are its subprocesses, and leaving them writing to session
	// files with nothing left to address them would be worse than stopping.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup := server.NewSupervisor()
	fmt.Fprintf(os.Stderr, "tau server listening on %s\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "warning: any client that can reach it can run tools and shell commands as you")

	if err := server.Serve(ctx, sup, ln); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
