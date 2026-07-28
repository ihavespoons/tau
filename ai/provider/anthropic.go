package provider

import (
	"context"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/anthropic"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/auth/oauth"
)

// AnthropicAuth returns the provider's auth handlers: API key (with
// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY resolution) plus the Claude
// Pro/Max OAuth flow.
func AnthropicAuth() auth.ProviderAuth {
	return auth.ProviderAuth{
		APIKey: auth.AnthropicAPIKeyAuth(),
		OAuth:  oauth.NewAnthropic(),
	}
}

// Anthropic builds the Anthropic provider. Auth is resolved per request via
// the supplied store and env context (Pi's Models.stream applyAuth step):
// stored credential first — refreshing OAuth inside the store lock — then
// ambient env.
func Anthropic(store auth.CredentialStore, env auth.EnvContext) *Provider {
	p := &Provider{
		ID: "anthropic", Name: "Anthropic", Api: ai.ApiAnthropicMessages,
		BaseURL: anthropicBaseURL,
		EnvKeys: []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
		Models:  AnthropicModels(),
	}

	// apply resolves credentials into opts and may redirect the request to an
	// auth- or env-supplied base URL (Pi's applyAuth + baseUrl swap).
	apply := func(ctx context.Context, model *ai.Model, opts *ai.StreamOptions) (*ai.Model, error) {
		if base := env.Env("ANTHROPIC_BASE_URL"); base != "" && model.BaseURL == anthropicBaseURL {
			m := *model
			m.BaseURL = base
			model = &m
		}
		if opts.APIKey != "" {
			return model, nil // explicit key wins; no resolution needed
		}
		res, err := auth.Resolve(ctx, p.ID, AnthropicAuth(), store, env, nil)
		if err != nil {
			return model, err
		}
		if res == nil {
			return model, &auth.Error{
				Code:    auth.CodeAuth,
				Message: "no Anthropic credentials: run `tau login`, or set ANTHROPIC_API_KEY",
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
		if res.Auth.BaseURL != "" {
			m := *model
			m.BaseURL = res.Auth.BaseURL
			model = &m
		}
		return model, nil
	}

	p.StreamSimple = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.SimpleStreamOptions{}
		}
		model, err := apply(ctx, model, &opts.StreamOptions)
		if err != nil {
			return errStream(model, err)
		}
		return anthropic.StreamSimple(ctx, model, c, opts)
	}

	p.Stream = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.StreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.StreamOptions{}
		}
		model, err := apply(ctx, model, opts)
		if err != nil {
			return errStream(model, err)
		}
		return anthropic.Stream(ctx, model, c, &anthropic.Options{StreamOptions: *opts})
	}

	return p
}

// errStream honors the never-throw contract: auth failures surface as a
// terminal error event, not an out-of-band error.
func errStream(model *ai.Model, err error) *ai.MessageStream {
	s := ai.NewMessageStream()
	msg := &ai.AssistantMessage{
		Content: ai.ContentList{}, Api: model.Api, Provider: model.Provider, Model: model.ID,
	}
	s.Push(ai.Event{Type: ai.EventError, Reason: ai.StopError,
		Error: ai.ErrorMessage(msg, ai.StopError, err.Error())})
	return s
}
