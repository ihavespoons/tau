package wire

// The protocol's TypeScript declarations are generated from the Go types in
// this package, and copied into the npm shim so it ships with them. CI runs
// `gen-wire-ts -check`, which fails when either copy has drifted.

//go:generate go run ../../cmd/gen-wire-ts -src . -o protocol.d.ts
//go:generate go run ../../cmd/gen-wire-ts -src . -o ../../shim/types/protocol.d.ts
