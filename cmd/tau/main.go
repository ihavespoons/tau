package main

import (
	"fmt"
	"os"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Printf("tau %s (%s, %s)\n", version, commit, date)
			return
		}
	}
	fmt.Fprintln(os.Stderr, "tau is under construction — see https://github.com/ihavespoons/tau")
	os.Exit(1)
}
