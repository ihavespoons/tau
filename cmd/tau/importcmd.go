package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ihavespoons/tau/config"
	interop "github.com/ihavespoons/tau/interop/pi"
)

// importCmd is `tau import`: adopt a Pi installation.
//
// Nothing is copied without being asked for. The default run is a report of
// what is there, because the two things worth importing — a credential file
// and several hundred sessions — are exactly the two you would not want a
// command to move on your behalf because you typed it to see what it did.
func importCmd(args []string) error {
	fs := flag.NewFlagSet("tau import", flag.ContinueOnError)
	var (
		source    = fs.String("from", config.PiAgentDir(), "Pi agent directory to import from")
		all       = fs.Bool("all", false, "import sessions, settings, models, credentials and resources")
		sessions  = fs.Bool("sessions", false, "import session history")
		settings  = fs.Bool("settings", false, "import settings.json")
		models    = fs.Bool("models", false, "import models.json and the cached catalogs")
		auth      = fs.Bool("auth", false, "import stored provider credentials")
		resources = fs.Bool("resources", false, "import skills, prompts, themes and extensions")
		overwrite = fs.Bool("overwrite", false, "replace files that already exist in tau")
		dryRun    = fs.Bool("dry-run", false, "report what would be imported without writing")
		yes       = fs.Bool("yes", false, "do not ask for confirmation")
	)
	fs.Usage = func() {
		fmt.Println("usage: tau import [flags]")
		fmt.Println("\nWith no flags, reports what a Pi installation contains.")
		fmt.Println("\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	snapshot, err := interop.Inspect(*source)
	if err != nil {
		return err
	}
	fmt.Print(snapshot.Describe())
	if snapshot.Empty() {
		return nil
	}

	opts := interop.ImportOptions{
		Source:      *source,
		AgentDir:    config.AgentDir(),
		SessionsDir: config.SessionsDir(),
		Sessions:    *sessions || *all,
		Settings:    *settings || *all,
		Models:      *models || *all,
		Auth:        *auth || *all,
		Resources:   *resources || *all,
		Overwrite:   *overwrite,
		DryRun:      *dryRun,
	}

	if !opts.Sessions && !opts.Settings && !opts.Models && !opts.Auth && !opts.Resources {
		fmt.Println("\nNothing selected. Choose what to import:")
		fmt.Println("  tau import --sessions          history only")
		fmt.Println("  tau import --all               everything, including credentials")
		fmt.Println("  tau import --all --dry-run     see what that would do first")
		return nil
	}

	if !*dryRun && !*yes {
		ok, err := confirmImport(opts)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Cancelled. Nothing was written.")
			return nil
		}
	}

	report, err := interop.Import(opts)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Print(report.Describe())

	if opts.Auth && !opts.DryRun {
		// Worth saying out loud: the import just put live tokens somewhere new.
		fmt.Println("\nCredentials were copied to " + config.AuthPath() + " (0600).")
		fmt.Println("Pi's own copy is untouched; revoking one does not revoke the other.")
	}
	return nil
}

// confirmImport asks before writing, listing what is about to happen.
func confirmImport(opts interop.ImportOptions) (bool, error) {
	var what []string
	if opts.Sessions {
		what = append(what, "session history")
	}
	if opts.Settings {
		what = append(what, "settings")
	}
	if opts.Models {
		what = append(what, "models")
	}
	if opts.Auth {
		what = append(what, "provider credentials")
	}
	if opts.Resources {
		what = append(what, "skills, prompts, themes and extensions")
	}

	fmt.Printf("\nImport %s\n", strings.Join(what, ", "))
	fmt.Printf("  from %s\n", opts.Source)
	fmt.Printf("  into %s\n", opts.AgentDir)
	if opts.Overwrite {
		fmt.Println("  replacing files that already exist in tau")
	} else {
		fmt.Println("  leaving any file that already exists in tau alone")
	}
	fmt.Print("\nProceed? [y/N] ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
