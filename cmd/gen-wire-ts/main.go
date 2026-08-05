// Command gen-wire-ts generates the TypeScript declarations for the subprocess
// extension protocol from the Go types that define it.
//
// # Why generate rather than hand-write
//
// The protocol has two implementations that must agree exactly: tau's host, in
// Go, and the npm host shim, in TypeScript. Two hand-written definitions of the
// same wire format drift, and the drift is invisible until an extension gets a
// field it does not expect — at which point the symptom is a silently ignored
// value, not a compile error.
//
// So the Go structs in extension/wire are the definition, this generates the
// TypeScript view of them, and CI fails if the checked-in output differs from
// what this produces. Changing the protocol without regenerating is caught at
// review time rather than at an extension author's desk.
//
// # Why go/ast rather than reflection
//
// Reflection sees types and JSON tags but not comments, and the comments are
// most of the value: an extension author reading protocol.d.ts should learn why
// tool_call fails closed, not just that a field called `block` exists.
//
// Usage:
//
//	go run ./cmd/gen-wire-ts -o extension/wire/protocol.d.ts
//	go run ./cmd/gen-wire-ts -check      # exit non-zero if the file is stale
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

func main() {
	var (
		src   = flag.String("src", "extension/wire", "directory holding the protocol types")
		out   = flag.String("o", "extension/wire/protocol.d.ts", "output file")
		check = flag.Bool("check", false, "verify the output is up to date instead of writing it")
	)
	flag.Parse()

	got, err := generate(*src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-wire-ts:", err)
		os.Exit(1)
	}

	if *check {
		want, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen-wire-ts: %s is missing; run `go generate ./extension/wire/`\n", *out)
			os.Exit(1)
		}
		if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
			fmt.Fprintf(os.Stderr,
				"gen-wire-ts: %s is out of date with the Go types; run `go generate ./extension/wire/`\n", *out)
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(*out, got, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-wire-ts:", err)
		os.Exit(1)
	}
}

// tsType maps a Go type expression to its TypeScript equivalent.
//
// The mapping is deliberately narrow. Anything it does not recognize becomes
// `unknown` rather than a guess: a wrong type in a .d.ts is worse than an
// unhelpful one, because it makes the compiler vouch for something false.
func tsType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string", "FrameType":
			return "string"
		case "bool":
			return "boolean"
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64",
			"float32", "float64":
			return "number"
		case "any":
			return "unknown"
		}
		// A named struct from this package keeps its name; the declaration
		// order below makes sure it is emitted too.
		return t.Name

	case *ast.StarExpr:
		// A Go pointer is an optional value. The optionality is expressed by
		// the `?` on the field, so the type itself is the pointee — except
		// that a pointer also distinguishes "absent" from "zero", which is
		// why `| null` is kept: an explicit null is how a header is deleted.
		return tsType(t.X) + " | null"

	case *ast.ArrayType:
		if id, ok := t.Elt.(*ast.Ident); ok && id.Name == "byte" {
			// []byte and json.RawMessage both reach here; both are raw JSON.
			return "unknown"
		}
		return tsType(t.Elt) + "[]"

	case *ast.MapType:
		return "Record<" + tsType(t.Key) + ", " + tsType(t.Value) + ">"

	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok && pkg.Name == "json" && t.Sel.Name == "RawMessage" {
			return "unknown"
		}
		return "unknown"

	case *ast.InterfaceType:
		return "unknown"
	}
	return "unknown"
}

// jsonName returns the wire name for a field and whether it is optional.
func jsonName(f *ast.Field, name string) (string, bool, bool) {
	if f.Tag == nil {
		// An untagged exported field still marshals, under its Go name. That
		// is almost always an oversight, so it is skipped rather than emitted
		// with a name the TypeScript side would have to guess.
		return "", false, false
	}
	tag, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return "", false, false
	}
	jsonTag := reflectTag(tag, "json")
	if jsonTag == "-" {
		return "", false, false
	}
	parts := strings.Split(jsonTag, ",")
	wire := parts[0]
	if wire == "" {
		wire = name
	}
	optional := false
	for _, p := range parts[1:] {
		if p == "omitempty" {
			optional = true
		}
	}
	return wire, optional, true
}

// reflectTag reads one key out of a struct tag without importing reflect's
// StructTag, which would need the quotes handled the same way anyway.
func reflectTag(tag, key string) string {
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]

		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		value, err := strconv.Unquote(tag[:i+1])
		if err != nil {
			break
		}
		tag = tag[i+1:]
		if name == key {
			return value
		}
	}
	return ""
}

func comment(doc *ast.CommentGroup, indent string) string {
	if doc == nil {
		return ""
	}
	text := strings.TrimSpace(doc.Text())
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(indent + "/**\n")
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(strings.TrimRight(indent+" * "+line, " ") + "\n")
	}
	b.WriteString(indent + " */\n")
	return b.String()
}

func generate(dir string) ([]byte, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		// Generated declarations describe the protocol, not the reference
		// server or its tests.
		name := fi.Name()
		return strings.HasSuffix(name, ".go") &&
			!strings.HasSuffix(name, "_test.go") &&
			name != "serve.go" && name != "codec.go"
	}, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString(`// Code generated by cmd/gen-wire-ts. DO NOT EDIT.
//
// These declarations are generated from the Go types in extension/wire, which
// are the definition of the subprocess extension protocol. Editing this file
// makes the two sides disagree without anything failing to compile; change the
// Go types and regenerate instead.
//
//   go generate ./extension/wire/

`)

	type decl struct {
		name string
		body string
	}
	var consts []string
	var types []decl

	for _, pkg := range pkgs {
		files := make([]string, 0, len(pkg.Files))
		for name := range pkg.Files {
			files = append(files, name)
		}
		// Map iteration order is unspecified, and a generator whose output
		// depends on it fails the CI check at random.
		sort.Strings(files)

		for _, name := range files {
			file := pkg.Files[name]
			for _, d := range file.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				switch gd.Tok {
				case token.CONST:
					consts = append(consts, emitConsts(gd)...)
				case token.TYPE:
					for _, spec := range gd.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || !ts.Name.IsExported() {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							continue
						}
						doc := gd.Doc
						if doc == nil {
							doc = ts.Doc
						}
						types = append(types, decl{ts.Name.Name, emitInterface(ts.Name.Name, doc, st)})
					}
				}
			}
		}
	}

	if len(consts) > 0 {
		b.WriteString("/** Frame type names, as they appear on the wire. */\n")
		b.WriteString("export const enum FrameName {\n")
		for _, c := range consts {
			b.WriteString(c)
		}
		b.WriteString("}\n\n")
	}

	sort.Slice(types, func(i, j int) bool { return types[i].name < types[j].name })
	for _, t := range types {
		b.WriteString(t.body)
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// emitConsts turns the FrameType constants into a TypeScript enum, so the shim
// cannot misspell a frame name.
func emitConsts(gd *ast.GenDecl) []string {
	var out []string
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			continue
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || id.Name != "FrameType" {
			continue
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		name := strings.TrimPrefix(vs.Names[0].Name, "Frame")
		out = append(out, fmt.Sprintf("\t%s = %q,\n", name, value))
	}
	return out
}

func emitInterface(name string, doc *ast.CommentGroup, st *ast.StructType) string {
	var b strings.Builder
	b.WriteString(comment(doc, ""))
	fmt.Fprintf(&b, "export interface %s {\n", name)
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			continue // embedded fields are not used in this protocol
		}
		for _, n := range f.Names {
			if !n.IsExported() {
				continue
			}
			wire, optional, ok := jsonName(f, n.Name)
			if !ok {
				continue
			}
			b.WriteString(comment(f.Doc, "\t"))
			q := ""
			if optional {
				q = "?"
			}
			fmt.Fprintf(&b, "\t%s%s: %s;\n", wire, q, tsType(f.Type))
		}
	}
	b.WriteString("}\n")
	return b.String()
}
