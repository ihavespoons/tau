// Package tau carries the repository documents the binary has to bring with
// it. A single static binary has no install tree beside it to read from, so
// anything tau tells the user about itself is compiled in.
//
// The embed lives at the module root because go:embed cannot reach outside its
// own directory, and CHANGELOG.md belongs at the root, where GitHub and anyone
// browsing the repository expect it. Nothing under this directory imports this
// package: the data is handed down from cmd/tau, which leaves the root free to
// grow into the embedder-facing facade without an import cycle.
package tau

import _ "embed"

// Changelog is the raw contents of CHANGELOG.md. The changelog package turns
// it into entries.
//
//go:embed CHANGELOG.md
var Changelog string
