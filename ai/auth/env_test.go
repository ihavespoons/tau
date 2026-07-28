package auth

import "testing"

func TestAnthropicResolveEnvPrecedence(t *testing.T) {
	a := AnthropicAPIKeyAuth()

	// ANTHROPIC_AUTH_TOKEN wins and becomes a bearer header, never an apiKey.
	res, err := a.Resolve(MapContext{
		AnthropicAuthTokenEnv:  "auth-token",
		AnthropicOAuthTokenEnv: "oauth-token",
		AnthropicAPIKeyEnv:     "api-key",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "" {
		t.Errorf("auth token must not be an apiKey: %+v", res.Auth)
	}
	if h := res.Auth.Headers["Authorization"]; h == nil || *h != "Bearer auth-token" {
		t.Errorf("headers = %+v", res.Auth.Headers)
	}
	if res.Source != AnthropicAuthTokenEnv {
		t.Errorf("source = %q", res.Source)
	}

	// Without it, OAUTH_TOKEN beats API_KEY and is OAuth-shaped api auth.
	res, err = a.Resolve(MapContext{
		AnthropicOAuthTokenEnv: "oauth-token",
		AnthropicAPIKeyEnv:     "api-key",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "oauth-token" || len(res.Auth.Headers) != 0 {
		t.Errorf("auth = %+v", res.Auth)
	}
	if res.Source != AnthropicOAuthTokenEnv {
		t.Errorf("source = %q", res.Source)
	}

	// API key last.
	res, _ = a.Resolve(MapContext{AnthropicAPIKeyEnv: "api-key"}, nil)
	if res.Auth.APIKey != "api-key" || res.Source != AnthropicAPIKeyEnv {
		t.Errorf("res = %+v", res)
	}

	// Nothing configured → nil result, nil error (Pi's undefined).
	res, err = a.Resolve(MapContext{}, nil)
	if res != nil || err != nil {
		t.Errorf("unconfigured = (%v, %v)", res, err)
	}
}

func TestAnthropicResolveStoredCredentialWins(t *testing.T) {
	a := AnthropicAPIKeyAuth()
	res, err := a.Resolve(
		MapContext{AnthropicAuthTokenEnv: "env-token", AnthropicAPIKeyEnv: "env-key"},
		&Credential{Type: CredentialAPIKey, Key: "stored-key", Env: ProviderEnv{"X": "1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.Auth.APIKey != "stored-key" || res.Source != "stored credential" {
		t.Errorf("res = %+v", res)
	}
	if res.Env["X"] != "1" {
		t.Errorf("provider env dropped: %+v", res.Env)
	}
}

func TestBlankEnvValuesIgnored(t *testing.T) {
	a := AnthropicAPIKeyAuth()
	res, _ := a.Resolve(MapContext{AnthropicAuthTokenEnv: "   ", AnthropicAPIKeyEnv: "real"}, nil)
	if res == nil || res.Auth.APIKey != "real" {
		t.Errorf("res = %+v", res)
	}
}

func TestFindEnvKeysAndEnvAPIKey(t *testing.T) {
	ctx := MapContext{AnthropicAuthTokenEnv: "t", AnthropicAPIKeyEnv: "k"}
	found := FindEnvKeys("anthropic", ctx)
	if len(found) != 2 || found[0] != AnthropicAuthTokenEnv || found[1] != AnthropicAPIKeyEnv {
		t.Errorf("found = %v", found)
	}
	// EnvAPIKey skips the bearer-only variable.
	if got := EnvAPIKey("anthropic", ctx); got != "k" {
		t.Errorf("EnvAPIKey = %q, want the API key", got)
	}
	// Only the bearer var set → no usable api key.
	if got := EnvAPIKey("anthropic", MapContext{AnthropicAuthTokenEnv: "t"}); got != "" {
		t.Errorf("EnvAPIKey = %q, want empty", got)
	}
	if got := EnvAPIKey("openai", MapContext{"OPENAI_API_KEY": "sk"}); got != "sk" {
		t.Errorf("openai EnvAPIKey = %q", got)
	}
	if FindEnvKeys("unknown-provider", ctx) != nil {
		t.Error("unknown provider should have no env keys")
	}
}

func TestWithEnvOverlay(t *testing.T) {
	base := MapContext{"A": "base", "B": "base"}
	ov := WithEnvOverlay(base, ProviderEnv{"A": "over"})
	if ov.Env("A") != "over" || ov.Env("B") != "base" {
		t.Errorf("overlay: A=%q B=%q", ov.Env("A"), ov.Env("B"))
	}
	if WithEnvOverlay(base, nil) == nil {
		t.Error("nil overlay should return base")
	}
}

func TestDefaultConfigValue(t *testing.T) {
	t.Setenv("TAU_TEST_KEY", "from-process")
	cases := []struct {
		in   string
		env  ProviderEnv
		want string
	}{
		{"literal", nil, "literal"},
		{"$TAU_TEST_KEY", nil, "from-process"},
		{"${TAU_TEST_KEY}", nil, "from-process"},
		{"prefix-$TAU_TEST_KEY-suffix", nil, "prefix-from-process-suffix"},
		{"$TAU_TEST_KEY", ProviderEnv{"TAU_TEST_KEY": "from-provider"}, "from-provider"},
		{"$$literal", nil, "$literal"},
		{"$!notacommand", nil, "!notacommand"},
		{"$UNSET_VAR_XYZ", nil, ""},
		{"!op read x", nil, "!op read x"}, // command indirection passed through
	}
	for _, c := range cases {
		if got := DefaultConfigValue(c.in, c.env); got != c.want {
			t.Errorf("DefaultConfigValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
