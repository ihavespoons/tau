package main

import (
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extensions/mcp"
)

// bundledExtensions are the extensions compiled into the tau binary.
//
// MCP ships this way on purpose: it is the acceptance test for the extension
// API. It is written against the public `extension` package and nothing else,
// so if it ever needs a private hook, that is a signal the API is too small
// rather than a licence to widen the core.
func bundledExtensions() []extension.Extension {
	return []extension.Extension{mcp.New()}
}
