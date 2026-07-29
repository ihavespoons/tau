package provider

import (
	"context"

	"github.com/ihavespoons/tau/ai"
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

	providerAuth := auth.ProviderAuth{
		APIKey: auth.EnvAPIKeyAuth(o.ID, p.Name+" API key", o.EnvKeys, o.ConfiguredKey),
	}

	// apply resolves credentials per request, exactly as the anthropic
	// provider does: stored credential first, then models.json, then ambient
	// environment.
	apply := func(ctx context.Context, model *ai.Model, opts *ai.StreamOptions) error {
		if opts.APIKey != "" {
			return nil // an explicit key wins; no resolution needed
		}
		res, err := auth.Resolve(ctx, o.ID, providerAuth, store, env, nil)
		if err != nil {
			return err
		}
		if res == nil {
			hint := "set an API key in ~/.tau/agent/models.json"
			if len(o.EnvKeys) > 0 {
				hint = "set " + o.EnvKeys[0]
			}
			return &auth.Error{
				Code:    auth.CodeAuth,
				Message: "no credentials for " + o.ID + ": " + hint,
			}
		}
		opts.APIKey = res.Auth.APIKey
		if len(res.Auth.Headers) > 0 {
			if opts.Headers == nil {
				opts.Headers = map[string]*string{}
			}
			for k, v := range res.Auth.Headers {
				if _, set := opts.Headers[k]; !set {
					opts.Headers[k] = v
				}
			}
		}
		return nil
	}

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
