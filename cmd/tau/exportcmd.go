package main

import (
	"flag"
	"fmt"

	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/export"
	"github.com/ihavespoons/tau/settings"
	"github.com/ihavespoons/tau/theme"
)

// exportCmd renders a session file to a self-contained HTML page.
//
// It works on a file rather than a running session, so it needs no provider,
// no credentials and no network: the page is built entirely from what the
// session recorded.
func exportCmd(args []string) error {
	fs := flag.NewFlagSet("tau export", flag.ContinueOnError)
	themeName := fs.String("theme", "", "theme to render with (default: the configured theme)")
	fs.Usage = func() {
		fmt.Println("usage: tau export <session-file> [output.html]")
		fmt.Println("\nRenders a session to a single HTML file that opens without a server.")
		fmt.Println("\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		fs.Usage()
		return fmt.Errorf("no session file given")
	case len(rest) > 2:
		return fmt.Errorf("expected a session file and an optional output path")
	}

	ctx, cancel := ctxWithSignals()
	defer cancel()

	data, err := export.FromFile(ctx, rest[0])
	if err != nil {
		return err
	}
	out := ""
	if len(rest) == 2 {
		out = rest[1]
	}

	path, err := export.WriteFile(data, exportTheme(*themeName), rest[0], out)
	if err != nil {
		return err
	}
	fmt.Println("Exported to: " + path)
	return nil
}

// exportTheme resolves the theme to render with: the name given on the command
// line, otherwise the configured one. Nil means the built-in dark theme.
//
// Only global settings are consulted. This command takes a path and renders
// it, with no project directory in play and so no trust decision to make; a
// checkout does not get to restyle a page just because it was the working
// directory when the command ran.
func exportTheme(name string) *theme.Theme {
	if name == "" {
		// ProjectTrusted is left false, so the merged view is the global one.
		if mgr, err := settings.Load(settings.Options{}); err == nil {
			if set, err := mgr.Resolve(); err == nil {
				name = set.ThemeSetting()
			}
		}
	}
	set := theme.Discover(theme.Options{Dir: config.ThemesDir()})
	th, ok := set.Resolve(name, theme.DetectBackground(nil).Mode)
	if !ok {
		return nil
	}
	return th
}
