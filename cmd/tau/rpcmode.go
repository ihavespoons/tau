package main

import (
	"context"
	"os"

	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/rpc"
)

// runRPCMode starts a session driven by JSONL commands on stdin.
//
// The extension UI is attached before the session is built, not after: an
// extension may open a dialog from its factory, and a UI installed afterwards
// would have missed it. That means the server object exists before the session
// it serves, which is why its session pointer is filled in rather than passed.
func runRPCMode(ctx context.Context, opts coding.Options, loader *subprocessLoader, prompt string) error {
	srv := &coding.RPCServer{}
	opts.UI = srv.UI()
	// An extension's diagnostics belong on the client's stream in this mode:
	// stderr is where a terminal user looks, and there is no terminal user.
	loader.SetLogSink(srv.EmitExtensionLog)

	cs, err := coding.New(ctx, opts)
	if err != nil {
		return err
	}
	defer cs.Close(ctx, "exit")

	srv.Attach(cs, os.Stdout)

	// A prompt on the command line is delivered as though a client had sent
	// one, so `tau --mode rpc "do the thing"` starts working immediately and
	// the client still sees every event.
	if prompt != "" {
		srv.Dispatch(ctx, rpc.Command{Type: "prompt", Message: prompt})
	}

	return srv.Serve(ctx, os.Stdin)
}
