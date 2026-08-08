package main

import (
	"bytes"
	"os"
	"testing"
)

// The protocol has two implementations that must agree: tau's Go host and the
// npm host shim. This is the gate that keeps them from drifting — a change to
// the Go types without a regenerated .d.ts fails here, not at an extension
// author's desk.
func TestGeneratedDeclarationsAreUpToDate(t *testing.T) {
	got, err := generate("../../extension/wire")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Both copies: the one Go developers read and the one the npm package
	// ships. A shim published against a stale declaration is the drift this
	// whole mechanism exists to prevent.
	for _, path := range []string{
		"../../extension/wire/protocol.d.ts",
		"../../shim/types/protocol.d.ts",
	} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
			t.Fatalf("%s is stale; run `go generate ./extension/wire/`", path)
		}
	}
}

// A generator whose output depends on map iteration order fails CI at random.
func TestGenerationIsDeterministic(t *testing.T) {
	first, err := generate("../../extension/wire")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for i := 0; i < 8; i++ {
		next, err := generate("../../extension/wire")
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !bytes.Equal(first, next) {
			t.Fatal("two runs produced different output")
		}
	}
}

// Comments are most of the value of the generated file: an author reading it
// should learn why tool_call fails closed, not only that a field exists.
func TestCommentsCrossOver(t *testing.T) {
	got, err := generate("../../extension/wire")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"fails CLOSED",
		"no opinion",
		"The extension is not loaded.",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("generated output lost the comment containing %q", want)
		}
	}
}
