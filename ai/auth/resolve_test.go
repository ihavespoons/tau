package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOAuth is a scripted OAuthAuth for resolution tests.
type fakeOAuth struct {
	refreshes atomic.Int32
	refreshFn func(cred *Credential) (*Credential, error)
	toAuthErr error
}

func (f *fakeOAuth) Name() string { return "fake" }

func (f *fakeOAuth) Login(context.Context, Interaction) (*Credential, error) {
	return nil, errors.New("not used")
}

func (f *fakeOAuth) Refresh(_ context.Context, cred *Credential) (*Credential, error) {
	f.refreshes.Add(1)
	if f.refreshFn != nil {
		return f.refreshFn(cred)
	}
	return &Credential{Type: CredentialOAuth, OAuth: &OAuthData{
		Access:  "refreshed-access",
		Refresh: "refreshed-refresh",
		Expires: nowMS() + time.Hour.Milliseconds(),
	}}, nil
}

func (f *fakeOAuth) ToAuth(cred *Credential) (ModelAuth, error) {
	if f.toAuthErr != nil {
		return ModelAuth{}, f.toAuthErr
	}
	return ModelAuth{APIKey: cred.OAuth.Access}, nil
}

func oauthCred(expiresIn time.Duration) *Credential {
	return &Credential{Type: CredentialOAuth, OAuth: &OAuthData{
		Access:  "current-access",
		Refresh: "current-refresh",
		Expires: nowMS() + expiresIn.Milliseconds(),
	}}
}

func TestResolveFreshOAuthTokenSkipsRefresh(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", oauthCred(time.Hour))
	flow := &fakeOAuth{}

	res, err := Resolve(ctx, "anthropic", ProviderAuth{OAuth: flow}, store, MapContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "current-access" || res.Source != "OAuth" {
		t.Errorf("res = %+v", res)
	}
	if n := flow.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0", n)
	}
}

// TestResolveConcurrentRefreshHappensOnce is the load-bearing test: five
// concurrent resolutions of an expired token must refresh exactly once and
// persist the rotated credential.
func TestResolveConcurrentRefreshHappensOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", oauthCred(time.Minute)) // inside the 5-min window
	flow := &fakeOAuth{refreshFn: func(*Credential) (*Credential, error) {
		time.Sleep(5 * time.Millisecond) // widen the race window
		return &Credential{Type: CredentialOAuth, OAuth: &OAuthData{
			Access:  "refreshed-access",
			Refresh: "refreshed-refresh",
			Expires: nowMS() + time.Hour.Milliseconds(),
		}}, nil
	}}

	var wg sync.WaitGroup
	results := make([]*AuthResult, 5)
	errs := make([]error, 5)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Resolve(ctx, "anthropic", ProviderAuth{OAuth: flow}, store, MapContext{}, nil)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if results[i].Auth.APIKey != "refreshed-access" {
			t.Errorf("resolve %d got %q", i, results[i].Auth.APIKey)
		}
	}
	if n := flow.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1", n)
	}
	stored, _ := store.Read(ctx, "anthropic")
	if stored.OAuth.Access != "refreshed-access" || stored.OAuth.Refresh != "refreshed-refresh" {
		t.Errorf("rotated credential not persisted: %+v", stored.OAuth)
	}
}

func TestResolveRefreshFailurePropagatesWithoutEnvFallback(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", oauthCred(time.Minute))
	flow := &fakeOAuth{refreshFn: func(*Credential) (*Credential, error) {
		return nil, errors.New("invalid_grant")
	}}

	res, err := Resolve(ctx, "anthropic",
		ProviderAuth{OAuth: flow, APIKey: AnthropicAPIKeyAuth()},
		store,
		MapContext{AnthropicAPIKeyEnv: "env-key"}, // must NOT be used
		nil)
	if res != nil {
		t.Fatalf("expected no result, got %+v (silent env fallback)", res)
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != CodeOAuth {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveLoggedOutDuringRefresh(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", oauthCred(time.Minute))
	flow := &fakeOAuth{}
	// Delete the credential before resolution takes the lock.
	if err := store.Delete(ctx, "anthropic"); err != nil {
		t.Fatal(err)
	}
	stale := oauthCred(time.Minute)
	res, err := resolveStoredOAuth(ctx, store, "anthropic", flow, stale, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("logged-out provider resolved to %+v", res)
	}
}

func TestResolveMinValidityRequirement(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", oauthCred(time.Minute))
	// Refresh returns a token that is still short-lived.
	flow := &fakeOAuth{refreshFn: func(*Credential) (*Credential, error) {
		return &Credential{Type: CredentialOAuth, OAuth: &OAuthData{
			Access: "short", Refresh: "r", Expires: nowMS() + (6 * time.Minute).Milliseconds(),
		}}, nil
	}}
	_, err := Resolve(ctx, "anthropic", ProviderAuth{OAuth: flow}, store, MapContext{},
		&Overrides{MinOAuthValidity: 30 * time.Minute})
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != CodeOAuth {
		t.Fatalf("err = %v, want oauth error about expiring too soon", err)
	}
}

func TestResolveStoredAPIKeyBeatsEnv(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", &Credential{Type: CredentialAPIKey, Key: "stored"})

	res, err := Resolve(ctx, "anthropic",
		ProviderAuth{APIKey: AnthropicAPIKeyAuth()},
		store, MapContext{AnthropicAPIKeyEnv: "env-key"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "stored" {
		t.Errorf("res = %+v, stored credential should own the provider", res)
	}
}

func TestResolveAmbientWhenNothingStored(t *testing.T) {
	ctx := context.Background()
	res, err := Resolve(ctx, "anthropic",
		ProviderAuth{APIKey: AnthropicAPIKeyAuth()},
		NewMemStore(), MapContext{AnthropicAPIKeyEnv: "env-key"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "env-key" || res.Source != AnthropicAPIKeyEnv {
		t.Errorf("res = %+v", res)
	}
}

func TestResolveOverrideAPIKeyShortCircuits(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", &Credential{Type: CredentialAPIKey, Key: "stored"})

	res, err := Resolve(ctx, "anthropic",
		ProviderAuth{APIKey: AnthropicAPIKeyAuth()},
		store, MapContext{}, &Overrides{APIKey: "explicit", HasAPIKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "explicit" {
		t.Errorf("res = %+v", res)
	}
}

func TestResolveStoredCredentialWithNoHandlerYieldsNothing(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	mustModify(t, store, "anthropic", oauthCred(time.Hour))
	// OAuth credential stored but provider offers only api-key auth.
	res, err := Resolve(ctx, "anthropic",
		ProviderAuth{APIKey: AnthropicAPIKeyAuth()},
		store, MapContext{AnthropicAPIKeyEnv: "env-key"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("res = %+v, want nil (no fallback for unhandled credential type)", res)
	}
}

func TestResolveUnconfigured(t *testing.T) {
	res, err := Resolve(context.Background(), "anthropic",
		ProviderAuth{APIKey: AnthropicAPIKeyAuth()}, NewMemStore(), MapContext{}, nil)
	if res != nil || err != nil {
		t.Errorf("(%v, %v), want (nil, nil)", res, err)
	}
}
