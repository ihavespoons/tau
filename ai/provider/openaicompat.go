package provider

import (
	"context"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/anthropic"
	"github.com/ihavespoons/tau/ai/api/openaichat"
	"github.com/ihavespoons/tau/ai/auth"
)

// OpenAICompatOptions describes a provider that speaks /v1/chat/completions.
//
// That is most of them: the wire is the same, so a provider is little more
// than an id, an endpoint, and where to find the key.
type OpenAICompatOptions struct {
	ID      string
	Name    string
	BaseURL string
	// EnvKeys are the environment variables that can supply an API key, most
	// specific first.
	EnvKeys []string
	Models  []ai.Model
	// ConfiguredKey is an apiKey value from models.json. It may be a literal
	// or a `$VAR` reference.
	ConfiguredKey string
}

// OpenAICompat builds a chat-completions provider.
//
// This is what makes a models.json entry usable: declare a base URL and an
// api of "openai-completions" and the provider becomes real, authenticated,
// and streamable without any code in tau naming it.
func OpenAICompat(store auth.CredentialStore, env auth.EnvContext, o OpenAICompatOptions) *Provider {
	p := &Provider{
		ID: o.ID, Name: o.Name, Api: ai.ApiOpenAICompletions,
		BaseURL: o.BaseURL, EnvKeys: o.EnvKeys, Models: o.Models,
	}
	if p.Name == "" {
		p.Name = o.ID
	}

	apply := keyResolver(store, env, o.ID, p.Name, o.EnvKeys, o.ConfiguredKey)

	p.StreamSimple = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.SimpleStreamOptions{}
		}
		if err := apply(ctx, model, &opts.StreamOptions); err != nil {
			return errStream(model, err)
		}
		return openaichat.StreamSimple(ctx, model, c, opts)
	}

	p.Stream = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.StreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.StreamOptions{}
		}
		if err := apply(ctx, model, opts); err != nil {
			return errStream(model, err)
		}
		return openaichat.Stream(ctx, model, c, &openaichat.Options{StreamOptions: *opts})
	}

	return p
}

// Auth returns a provider's authentication handlers, for the login flows.
func OpenAICompatAuth(providerID, name string, envKeys []string) auth.ProviderAuth {
	return auth.ProviderAuth{
		APIKey: auth.EnvAPIKeyAuth(providerID, name+" API key", envKeys, ""),
	}
}

// keyResolver builds the per-request credential step shared by the key-only
// providers: stored credential first, then models.json, then ambient
// environment. Anthropic proper does not use it — it has an OAuth flow and a
// base-URL redirect that do not fit this shape.
func keyResolver(store auth.CredentialStore, env auth.EnvContext, id, name string, envKeys []string, configuredKey string, oauthFlow ...auth.OAuthAuth) func(context.Context, *ai.Model, *ai.StreamOptions) error {
	providerAuth := auth.ProviderAuth{
		APIKey: auth.EnvAPIKeyAuth(id, name+" API key", envKeys, configuredKey),
	}
	// A provider may offer both a key and a login; resolution takes whichever
	// the user actually completed.
	for _, flow := range oauthFlow {
		if flow != nil {
			providerAuth.OAuth = flow
		}
	}

	return func(ctx context.Context, _ *ai.Model, opts *ai.StreamOptions) error {
		if opts.APIKey != "" {
			return nil // an explicit key wins; no resolution needed
		}
		res, err := auth.Resolve(ctx, id, providerAuth, store, env, nil)
		if err != nil {
			return err
		}
		if res == nil {
			hint := "set an API key in ~/.tau/agent/models.json"
			if len(envKeys) > 0 {
				hint = "set " + envKeys[0]
			}
			return &auth.Error{
				Code:    auth.CodeAuth,
				Message: "no credentials for " + id + ": " + hint,
				Cause:   auth.ErrNoCredentials,
			}
		}
		opts.APIKey = res.Auth.APIKey
		for k, v := range res.Auth.Headers {
			if opts.Headers == nil {
				opts.Headers = map[string]*string{}
			}
			if _, set := opts.Headers[k]; !set {
				opts.Headers[k] = v
			}
		}
		return nil
	}
}

// AnthropicCompatOptions describes a provider that speaks Anthropic's messages
// wire without being Anthropic — a gateway or a vendor that adopted the shape.
type AnthropicCompatOptions struct {
	ID      string
	Name    string
	BaseURL string
	EnvKeys []string
	Models  []ai.Model
	// ConfiguredKey is an apiKey value from models.json; a literal or a `$VAR`.
	ConfiguredKey string
}

// AnthropicCompat builds a key-authenticated provider on Anthropic's wire.
//
// It is deliberately separate from Anthropic(): that one carries an OAuth
// flow, a Claude-subscription base-URL redirect, and beta headers that belong
// to Anthropic's own endpoint and would be wrong to send anywhere else.
func AnthropicCompat(store auth.CredentialStore, env auth.EnvContext, o AnthropicCompatOptions) *Provider {
	p := &Provider{
		ID: ai.ProviderId(o.ID), Name: o.Name, Api: ai.ApiAnthropicMessages,
		BaseURL: o.BaseURL, EnvKeys: o.EnvKeys, Models: o.Models,
	}
	if p.Name == "" {
		p.Name = o.ID
	}

	apply := keyResolver(store, env, o.ID, p.Name, o.EnvKeys, o.ConfiguredKey)

	p.StreamSimple = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.SimpleStreamOptions{}
		}
		if err := apply(ctx, model, &opts.StreamOptions); err != nil {
			return errStream(model, err)
		}
		return anthropic.StreamSimple(ctx, model, c, opts)
	}

	p.Stream = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.StreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.StreamOptions{}
		}
		if err := apply(ctx, model, opts); err != nil {
			return errStream(model, err)
		}
		return anthropic.Stream(ctx, model, c, &anthropic.Options{StreamOptions: *opts})
	}

	return p
}
