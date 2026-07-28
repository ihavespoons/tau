package osenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent/env"
)

func newTestEnv(t *testing.T) (*OSEnv, string) {
	t.Helper()
	dir := t.TempDir()
	// macOS temp dirs are symlinked (/var -> /private/var); resolve so path
	// comparisons in tests are stable.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	e, err := New(Options{Cwd: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadWindowing(t *testing.T) {
	e, dir := newTestEnv(t)
	writeFile(t, filepath.Join(dir, "f.txt"), "l1\nl2\nl3\nl4\nl5")

	tests := []struct {
		name      string
		opts      env.ReadOptions
		want      string
		truncated bool
	}{
		{"whole file", env.ReadOptions{}, "l1\nl2\nl3\nl4\nl5", false},
		{"offset", env.ReadOptions{Offset: 3}, "l3\nl4\nl5", false},
		{"limit", env.ReadOptions{Limit: 2}, "l1\nl2", true},
		{"offset+limit", env.ReadOptions{Offset: 2, Limit: 2}, "l2\nl3", true},
		{"limit past end", env.ReadOptions{Offset: 4, Limit: 10}, "l4\nl5", false},
		{"byte cap stops on line boundary", env.ReadOptions{MaxBytes: 5}, "l1\nl2", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Read(context.Background(), "f.txt", tc.opts)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if res.Content != tc.want {
				t.Errorf("content = %q, want %q", res.Content, tc.want)
			}
			if res.Truncated != tc.truncated {
				t.Errorf("truncated = %v, want %v", res.Truncated, tc.truncated)
			}
			if res.TotalLines != 5 {
				t.Errorf("totalLines = %d, want 5", res.TotalLines)
			}
		})
	}
}

func TestReadOffsetBeyondEnd(t *testing.T) {
	e, dir := newTestEnv(t)
	writeFile(t, filepath.Join(dir, "f.txt"), "a\nb")

	res, err := e.Read(context.Background(), "f.txt", env.ReadOptions{Offset: 99})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Content != "" {
		t.Errorf("content = %q, want empty", res.Content)
	}
	if res.TotalLines != 2 {
		t.Errorf("totalLines = %d, want 2", res.TotalLines)
	}
}

// Binary content must be flagged, never returned as mangled text.
func TestReadDetectsBinary(t *testing.T) {
	e, dir := newTestEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte{0x00, 0xff, 0xfe, 0x41}, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.Read(context.Background(), "b.bin", env.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Binary {
		t.Error("expected Binary=true")
	}
	if res.Content != "" {
		t.Errorf("binary read returned text content %q", res.Content)
	}
}

func TestReadDetectsImagesByMagicBytes(t *testing.T) {
	e, dir := newTestEnv(t)

	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte{0, 0, 0, 13, 'I', 'H', 'D', 'R'}...)
	png = append(png, make([]byte, 16)...)
	gif := []byte("GIF89a" + strings.Repeat("\x00", 20))
	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 20)...)

	tests := []struct {
		name, file string
		data       []byte
		wantMime   string
	}{
		{"png", "a.png", png, "image/png"},
		{"gif", "a.gif", gif, "image/gif"},
		{"jpeg", "a.jpg", jpeg, "image/jpeg"},
		// Extension must not drive detection: a text file named .png is text.
		{"text named png", "fake.png", []byte("hello, not an image"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, tc.file), tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := e.Read(context.Background(), tc.file, env.ReadOptions{})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if res.MimeType != tc.wantMime {
				t.Errorf("mime = %q, want %q", res.MimeType, tc.wantMime)
			}
			if tc.wantMime != "" && len(res.Bytes) != len(tc.data) {
				t.Errorf("bytes = %d, want %d", len(res.Bytes), len(tc.data))
			}
		})
	}
}

func TestFSErrorsAreTyped(t *testing.T) {
	e, dir := newTestEnv(t)

	if _, err := e.Read(context.Background(), "missing.txt", env.ReadOptions{}); !env.IsCode(err, env.CodeNotFound) {
		t.Errorf("Read missing: got %v, want CodeNotFound", err)
	}
	if _, err := e.ReadFile(context.Background(), "missing.txt"); !env.IsCode(err, env.CodeNotFound) {
		t.Errorf("ReadFile missing: got %v, want CodeNotFound", err)
	}
	if _, err := e.Stat(context.Background(), "missing.txt"); !env.IsCode(err, env.CodeNotFound) {
		t.Errorf("Stat missing: got %v, want CodeNotFound", err)
	}
	if _, err := e.List(context.Background(), "missing-dir"); !env.IsCode(err, env.CodeNotFound) {
		t.Errorf("List missing: got %v, want CodeNotFound", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Read(context.Background(), "d", env.ReadOptions{}); !env.IsCode(err, env.CodeNotAFile) {
		t.Errorf("Read dir: got %v, want CodeNotAFile", err)
	}
	if _, err := e.Abs(""); !env.IsCode(err, env.CodeInvalidPath) {
		t.Errorf("Abs empty: got %v, want CodeInvalidPath", err)
	}
}

func TestWriteCreatesParents(t *testing.T) {
	e, dir := newTestEnv(t)
	if err := e.Write(context.Background(), "a/b/c/f.txt", []byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a/b/c/f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Errorf("content = %q", got)
	}
}

func TestStatAndList(t *testing.T) {
	e, dir := newTestEnv(t)
	writeFile(t, filepath.Join(dir, "sub", "a.txt"), "aaa")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "bb")

	info, err := e.Stat(context.Background(), "sub/a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Name != "a.txt" || info.Size != 3 || info.IsDir {
		t.Errorf("stat = %+v", info)
	}

	entries, err := e.List(context.Background(), "sub")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if !filepath.IsAbs(entry.Path) {
			t.Errorf("entry path %q is not absolute", entry.Path)
		}
	}
}

func TestMkdirRemoveExists(t *testing.T) {
	e, _ := newTestEnv(t)
	ctx := context.Background()

	if e.Exists(ctx, "x/y") {
		t.Error("Exists should be false before Mkdir")
	}
	if err := e.Mkdir(ctx, "x/y"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if !e.Exists(ctx, "x/y") {
		t.Error("Exists should be true after Mkdir")
	}
	if err := e.Remove(ctx, "x/y"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if e.Exists(ctx, "x/y") {
		t.Error("Exists should be false after Remove")
	}
}

func TestAbsResolvesAgainstCwd(t *testing.T) {
	e, dir := newTestEnv(t)

	got, err := e.Abs("rel/path.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "rel/path.txt"); got != want {
		t.Errorf("Abs(rel) = %q, want %q", got, want)
	}

	if got, err := e.Abs("/tmp/abs.txt"); err != nil || got != "/tmp/abs.txt" {
		t.Errorf("Abs(abs) = %q, %v", got, err)
	}
	if got, err := e.Abs("file:///tmp/x.txt"); err != nil || got != "/tmp/x.txt" {
		t.Errorf("Abs(file://) = %q, %v", got, err)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if got, err := e.Abs("~/sub"); err != nil || got != filepath.Join(home, "sub") {
			t.Errorf("Abs(~) = %q, %v", got, err)
		}
	}
}
