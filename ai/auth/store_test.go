package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// piAuthJSON is a realistic ~/.pi/agent/auth.json: one OAuth entry (with an
// extra field Pi flows may write) and two api-key entries, one of which uses
// provider env and a $VAR indirection.
const piAuthJSON = `{
  "anthropic": {
    "type": "oauth",
    "refresh": "sk-ant-ort-refresh",
    "access": "sk-ant-oat-access",
    "expires": 1900000000000,
    "scope": "user:inference"
  },
  "openai": {
    "type": "api_key",
    "key": "sk-openai-literal"
  },
  "cloudflare-workers-ai": {
    "type": "api_key",
    "key": "$CF_TOKEN_FOR_TEST",
    "env": { "CLOUDFLARE_ACCOUNT_ID": "acct-123" }
  }
}`

func TestFileStoreReadsPiAuthJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(piAuthJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CF_TOKEN_FOR_TEST", "cf-secret")

	s := NewFileStore(path)
	ctx := context.Background()

	cred, err := s.Read(ctx, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if cred == nil || cred.Type != CredentialOAuth {
		t.Fatalf("anthropic cred = %+v", cred)
	}
	if cred.OAuth.Access != "sk-ant-oat-access" || cred.OAuth.Refresh != "sk-ant-ort-refresh" {
		t.Errorf("oauth tokens = %+v", cred.OAuth)
	}
	if cred.OAuth.Expires != 1900000000000 {
		t.Errorf("expires = %d", cred.OAuth.Expires)
	}
	if _, ok := cred.OAuth.Extra["scope"]; !ok {
		t.Errorf("extra fields dropped: %+v", cred.OAuth.Extra)
	}

	openai, err := s.Read(ctx, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if openai.Type != CredentialAPIKey || openai.Key != "sk-openai-literal" {
		t.Errorf("openai cred = %+v", openai)
	}

	cf, err := s.Read(ctx, "cloudflare-workers-ai")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Key != "cf-secret" {
		t.Errorf("indirection not resolved: %q", cf.Key)
	}
	if cf.Env["CLOUDFLARE_ACCOUNT_ID"] != "acct-123" {
		t.Errorf("provider env = %+v", cf.Env)
	}

	missing, err := s.Read(ctx, "nope")
	if err != nil || missing != nil {
		t.Errorf("missing entry = (%v, %v)", missing, err)
	}
}

func TestFileStoreRoundTripPreservesExtras(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(piAuthJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewFileStore(path)
	ctx := context.Background()

	// Touch an unrelated provider; the anthropic entry must survive verbatim.
	if _, err := s.Modify(ctx, "groq", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialAPIKey, Key: "gsk-x"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ant := doc["anthropic"]
	if ant["type"] != "oauth" || ant["access"] != "sk-ant-oat-access" || ant["scope"] != "user:inference" {
		t.Errorf("anthropic entry not preserved: %+v", ant)
	}
	if doc["groq"]["key"] != "gsk-x" {
		t.Errorf("groq entry = %+v", doc["groq"])
	}
}

func TestFileStorePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	path := filepath.Join(dir, "auth.json")
	s := NewFileStore(path)
	ctx := context.Background()

	if _, err := s.Modify(ctx, "anthropic", func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialAPIKey, Key: "k"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

// TestFileStoreModifySerializes asserts read-modify-write cycles never
// interleave: each closure sees the value the previous one wrote.
func TestFileStoreModifySerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := NewFileStore(path)
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Modify(ctx, "anthropic", func(cur *Credential) (*Credential, error) {
				count := 0
				if cur != nil && cur.Env != nil {
					if v, ok := cur.Env["count"]; ok {
						_, _ = fmtSscan(v, &count)
					}
				}
				// Yield inside the critical section to expose interleaving.
				time.Sleep(time.Millisecond)
				return &Credential{
					Type: CredentialAPIKey,
					Key:  "k",
					Env:  ProviderEnv{"count": fmtItoa(count + 1)},
				}, nil
			})
			if err != nil {
				t.Errorf("modify: %v", err)
			}
		}()
	}
	wg.Wait()

	final, err := s.Read(ctx, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if final.Env["count"] != fmtItoa(n) {
		t.Errorf("count = %s, want %d (lost updates: writes interleaved)", final.Env["count"], n)
	}
}

func TestModifyNilLeavesEntryUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store CredentialStore
	}{
		{"mem", NewMemStore()},
		{"file", NewFileStore(filepath.Join(t.TempDir(), "auth.json"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := tc.store.Modify(ctx, "p", func(*Credential) (*Credential, error) {
				return &Credential{Type: CredentialAPIKey, Key: "original"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := tc.store.Modify(ctx, "p", func(cur *Credential) (*Credential, error) {
				if cur == nil || cur.Key != "original" {
					t.Errorf("closure saw %+v", cur)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Key != "original" {
				t.Errorf("modify(nil) returned %+v, want current", got)
			}
			after, _ := tc.store.Read(ctx, "p")
			if after.Key != "original" {
				t.Errorf("entry changed: %+v", after)
			}
		})
	}
}

func TestStoreListAndDelete(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(filepath.Join(t.TempDir(), "auth.json"))
	mustModify(t, s, "anthropic", &Credential{Type: CredentialOAuth, OAuth: &OAuthData{Access: "a", Refresh: "r", Expires: 1}})
	mustModify(t, s, "openai", &Credential{Type: CredentialAPIKey, Key: "k"})

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ProviderID != "anthropic" || list[0].Type != CredentialOAuth {
		t.Errorf("list = %+v", list)
	}

	if err := s.Delete(ctx, "anthropic"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Read(ctx, "anthropic"); c != nil {
		t.Errorf("deleted credential still present: %+v", c)
	}
	list, _ = s.List(ctx)
	if len(list) != 1 {
		t.Errorf("list after delete = %+v", list)
	}
}

func TestReadStoredCredentialSkipsIndirection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(piAuthJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	c := ReadStoredCredential("cloudflare-workers-ai", path)
	if c == nil || c.Key != "$CF_TOKEN_FOR_TEST" {
		t.Errorf("cred = %+v (should be verbatim)", c)
	}
	if ReadStoredCredential("anthropic", "/no/such/file") != nil {
		t.Error("missing file should yield nil")
	}
}

func mustModify(t *testing.T, s CredentialStore, id string, c *Credential) {
	t.Helper()
	if _, err := s.Modify(context.Background(), id, func(*Credential) (*Credential, error) {
		return c, nil
	}); err != nil {
		t.Fatal(err)
	}
}
