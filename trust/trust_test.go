package trust

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func tempStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir), dir
}

// project builds a cwd containing a gated resource under .tau.
func project(t *testing.T, resource string) string {
	t.Helper()
	root := t.TempDir()
	// EvalSymlinks so the path matches what normalize() produces (macOS
	// /var -> /private/var), otherwise store keys will not line up.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(resolved, ".tau")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if resource != "" {
		if err := os.WriteFile(filepath.Join(cfg, resource), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return resolved
}

func ptr(b bool) *bool { return &b }

func TestHasGatedResources(t *testing.T) {
	for _, res := range gatedResources {
		t.Run(res, func(t *testing.T) {
			cwd := project(t, res)
			if !HasGatedResources(cwd, ".tau", "") {
				t.Errorf("%s under .tau should require trust", res)
			}
		})
	}

	t.Run("empty project needs no trust", func(t *testing.T) {
		cwd := project(t, "")
		if HasGatedResources(cwd, ".tau", "") {
			t.Error("a project with no gated resources should not require trust")
		}
	})

	t.Run("unrelated file needs no trust", func(t *testing.T) {
		cwd := project(t, "")
		if err := os.WriteFile(filepath.Join(cwd, ".tau", "notes.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if HasGatedResources(cwd, ".tau", "") {
			t.Error("an unrecognized file under .tau should not require trust")
		}
	})

	t.Run("ancestor .agents/skills requires trust", func(t *testing.T) {
		root := t.TempDir()
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(resolved, ".agents", "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(resolved, "a", "b")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if !HasGatedResources(nested, ".tau", "") {
			t.Error("an ancestor .agents/skills should require trust")
		}
	})

	t.Run("user home .agents/skills is exempt", func(t *testing.T) {
		home := t.TempDir()
		resolved, err := filepath.EvalSymlinks(home)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(resolved, ".agents", "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		// cwd IS the home dir: the user's own skills are a user resource.
		if HasGatedResources(resolved, ".tau", resolved) {
			t.Error("the user's own ~/.agents/skills must not trigger project trust")
		}
	})
}

func TestDecideOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("override wins over everything", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "settings.json")
		if err := store.Set(ctx, cwd, ptr(false)); err != nil {
			t.Fatal(err)
		}
		out, err := Decide(store, Request{
			Cwd: cwd, Override: ptr(true), Default: Never, ConfigDirName: ".tau",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Trusted {
			t.Error("an explicit override must win over a stored denial")
		}
	})

	t.Run("no gated resources is trusted", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "")
		out, err := Decide(store, Request{Cwd: cwd, Default: Never, ConfigDirName: ".tau"})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Trusted {
			t.Error("nothing to gate means trusted, even with default=never")
		}
	})

	t.Run("stored decision wins over default", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "extensions")
		if err := store.Set(ctx, cwd, ptr(true)); err != nil {
			t.Fatal(err)
		}
		out, err := Decide(store, Request{Cwd: cwd, Default: Never, ConfigDirName: ".tau"})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Trusted {
			t.Error("a stored trust decision should beat default=never")
		}
	})

	t.Run("stored denial wins over default always", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "extensions")
		if err := store.Set(ctx, cwd, ptr(false)); err != nil {
			t.Fatal(err)
		}
		out, err := Decide(store, Request{Cwd: cwd, Default: Always, ConfigDirName: ".tau"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Trusted {
			t.Error("a stored denial should beat default=always")
		}
	})

	t.Run("default always", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "skills")
		out, _ := Decide(store, Request{Cwd: cwd, Default: Always, ConfigDirName: ".tau"})
		if !out.Trusted || out.NeedsPrompt {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("default never", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "skills")
		out, _ := Decide(store, Request{Cwd: cwd, Default: Never, ConfigDirName: ".tau"})
		if out.Trusted || out.NeedsPrompt {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("ask without UI fails closed", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "settings.json")
		out, _ := Decide(store, Request{Cwd: cwd, Default: Ask, HasUI: false, ConfigDirName: ".tau"})
		if out.Trusted {
			t.Error("a non-interactive run must not trust an undecided project")
		}
		if out.NeedsPrompt {
			t.Error("cannot prompt without a UI")
		}
	})

	t.Run("ask with UI requests a prompt", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "settings.json")
		out, _ := Decide(store, Request{Cwd: cwd, Default: Ask, HasUI: true, ConfigDirName: ".tau"})
		if out.Trusted {
			t.Error("trust must not be granted before the user answers")
		}
		if !out.NeedsPrompt {
			t.Error("expected a prompt request")
		}
	})

	t.Run("empty default behaves as ask", func(t *testing.T) {
		store, _ := tempStore(t)
		cwd := project(t, "settings.json")
		out, _ := Decide(store, Request{Cwd: cwd, HasUI: false, ConfigDirName: ".tau"})
		if out.Trusted {
			t.Error("an unset default must not trust")
		}
	})
}

// Trusting a parent covers its descendants.
func TestAncestorInheritance(t *testing.T) {
	ctx := context.Background()
	store, _ := tempStore(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(resolved, "a", "b", "c")
	if err := os.MkdirAll(filepath.Join(child, ".tau"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, ".tau", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Set(ctx, resolved, ptr(true)); err != nil {
		t.Fatal(err)
	}
	out, err := Decide(store, Request{Cwd: child, Default: Never, ConfigDirName: ".tau"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Trusted {
		t.Error("trusting an ancestor should cover nested projects")
	}

	// A nearer explicit denial overrides the ancestor's trust.
	if err := store.Set(ctx, child, ptr(false)); err != nil {
		t.Fatal(err)
	}
	out, err = Decide(store, Request{Cwd: child, Default: Always, ConfigDirName: ".tau"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Trusted {
		t.Error("the nearest decision should win over an ancestor's")
	}
}

func TestStoreRoundTripAndRemoval(t *testing.T) {
	ctx := context.Background()
	store, dir := tempStore(t)
	cwd := project(t, "settings.json")

	if d, err := store.Lookup(cwd); err != nil || d != nil {
		t.Fatalf("expected no decision, got %v %v", d, err)
	}
	if err := store.Set(ctx, cwd, ptr(true)); err != nil {
		t.Fatal(err)
	}
	d, err := store.Lookup(cwd)
	if err != nil || d == nil || !d.Trusted {
		t.Fatalf("lookup = %v %v", d, err)
	}
	if err := store.Set(ctx, cwd, nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := store.Lookup(cwd); d != nil {
		t.Errorf("a nil decision should remove the entry, got %v", d)
	}

	info, err := os.Stat(filepath.Join(dir, "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("trust.json mode = %o, want 0600", perm)
	}
}

func TestStoreRejectsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trust.json"), []byte(`{"/x":"yes"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	if _, err := store.Lookup("/x"); err == nil {
		t.Error("expected an error for a non-boolean trust value")
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	store := NewStore(t.TempDir())
	d, err := store.Lookup("/anywhere")
	if err != nil {
		t.Fatalf("a missing trust store should not error: %v", err)
	}
	if d != nil {
		t.Errorf("d = %v", d)
	}
}

func TestSetManyIsAtomic(t *testing.T) {
	ctx := context.Background()
	store, _ := tempStore(t)
	root := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(root)
	child := filepath.Join(resolved, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	// The "trust parent" option trusts the parent and clears the child.
	if err := store.Set(ctx, child, ptr(false)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMany(ctx, map[string]*bool{resolved: ptr(true), child: nil}); err != nil {
		t.Fatal(err)
	}
	d, err := store.Lookup(child)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || !d.Trusted || d.Path != resolved {
		t.Errorf("lookup = %+v, want inherited trust from %s", d, resolved)
	}
}

func TestOptions(t *testing.T) {
	root := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(root)
	cwd := filepath.Join(resolved, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := Options(cwd, false)
	if len(opts) != 3 {
		t.Fatalf("got %d options, want trust/trust-parent/deny", len(opts))
	}
	if !opts[0].Trusted || opts[len(opts)-1].Trusted {
		t.Error("first option should trust, last should deny")
	}

	// The parent option must also clear any nearer decision, or the child's
	// own entry would keep shadowing the parent's.
	parentOpt := opts[1]
	if v, ok := parentOpt.Updates[cwd]; !ok || v != nil {
		t.Errorf("trust-parent should clear the child entry, updates = %v", parentOpt.Updates)
	}
	if v, ok := parentOpt.Updates[resolved]; !ok || v == nil || !*v {
		t.Errorf("trust-parent should trust %s", resolved)
	}

	withSession := Options(cwd, true)
	if len(withSession) != 5 {
		t.Errorf("got %d options with session-only variants, want 5", len(withSession))
	}
	for _, o := range withSession {
		if o.Label == "Trust (this session only)" && len(o.Updates) != 0 {
			t.Error("a session-only choice must not write to the store")
		}
	}
}
