package provider

import (
	"context"
	"fmt"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/anthropic"
	"github.com/ihavespoons/tau/ai/api/googlegenai"
	"github.com/ihavespoons/tau/ai/api/openaichat"
	"github.com/ihavespoons/tau/ai/api/openairesp"
	"github.com/ihavespoons/tau/ai/auth"
)

// KeyedOptions describes a provider authenticated by an API key alone.
type KeyedOptions struct {
	ID      string
	Name    string
	BaseURL string
	// EnvKeys are the environment variables that can supply the key, most
	// specific first.
	EnvKeys []string
	Models  []ai.Model
	// ConfiguredKey is an apiKey value from models.json: a literal or a `$VAR`.
	ConfiguredKey string
}

// Keyed builds a provider that dispatches each request to the wire its MODEL
// declares, not the wire the provider does.
//
// That distinction is load-bearing. A provider is not always one wire:
// Fireworks serves most of its catalog over an Anthropic-compatible endpoint
// but routes GLM 5.2 through chat-completions, and xAI serves Grok 4.5 over
// the responses API and everything else over chat-completions. Choosing the
// wire from the provider would send those models to the wrong parser and fail
// in a way that looks like the model is broken.
func Keyed(store auth.CredentialStore, env auth.EnvContext, o KeyedOptions) *Provider {
	p := &Provider{
		ID: ai.ProviderId(o.ID), Name: o.Name, BaseURL: o.BaseURL,
		EnvKeys: o.EnvKeys, Models: o.Models,
	}
	if p.Name == "" {
		p.Name = o.ID
	}
	// The provider-level Api is the one most of its models speak; it exists
	// for display and for callers that have no model in hand.
	p.Api = dominantApi(o.Models)

	apply := keyResolver(store, env, o.ID, p.Name, o.EnvKeys, o.ConfiguredKey)

	p.StreamSimple = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.SimpleStreamOptions{}
		}
		if err := apply(ctx, model, &opts.StreamOptions); err != nil {
			return errStream(model, err)
		}
		switch model.Api {
		case ai.ApiOpenAICompletions:
			return openaichat.StreamSimple(ctx, model, c, opts)
		case ai.ApiAnthropicMessages:
			return anthropic.StreamSimple(ctx, model, c, opts)
		case ai.ApiOpenAIResponses:
			return openairesp.StreamSimple(ctx, model, c, opts)
		case ai.ApiAzureOpenAIResponses:
			return openairesp.StreamSimpleAzure(ctx, model, c, opts)
		case ai.ApiGoogleGenerativeAI:
			return googlegenai.StreamSimple(ctx, model, c, opts)
		default:
			return errStream(model, unsupportedWire(model))
		}
	}

	p.Stream = func(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.StreamOptions) *ai.MessageStream {
		if opts == nil {
			opts = &ai.StreamOptions{}
		}
		if err := apply(ctx, model, opts); err != nil {
			return errStream(model, err)
		}
		switch model.Api {
		case ai.ApiOpenAICompletions:
			return openaichat.Stream(ctx, model, c, &openaichat.Options{StreamOptions: *opts})
		case ai.ApiAnthropicMessages:
			return anthropic.Stream(ctx, model, c, &anthropic.Options{StreamOptions: *opts})
		case ai.ApiOpenAIResponses:
			return openairesp.Stream(ctx, model, c, &openairesp.Options{StreamOptions: *opts})
		case ai.ApiAzureOpenAIResponses:
			return openairesp.StreamAzure(ctx, model, c,
				&openairesp.AzureOptions{Options: openairesp.Options{StreamOptions: *opts}})
		case ai.ApiGoogleGenerativeAI:
			return googlegenai.Stream(ctx, model, c, &googlegenai.Options{StreamOptions: *opts})
		default:
			return errStream(model, unsupportedWire(model))
		}
	}

	return p
}

// unsupportedWire names the model and the wire, because "not supported" alone
// leaves the user guessing whether the problem is the model, the key, or tau.
func unsupportedWire(model *ai.Model) error {
	return fmt.Errorf("%s speaks the %s API, which tau does not implement yet", model.ID, model.Api)
}

// dominantApi returns the wire most of a catalog speaks.
func dominantApi(models []ai.Model) ai.Api {
	counts := map[ai.Api]int{}
	for _, m := range models {
		counts[m.Api]++
	}
	best, bestN := ai.Api(""), 0
	for api, n := range counts {
		// Ties break on the api name so the result does not depend on map
		// iteration order.
		if n > bestN || (n == bestN && api < best) {
			best, bestN = api, n
		}
	}
	return best
}

// StreamableWires reports whether tau can talk to a model's wire at all.
func StreamableWire(api ai.Api) bool {
	switch api {
	case ai.ApiOpenAICompletions, ai.ApiAnthropicMessages,
		ai.ApiOpenAIResponses, ai.ApiAzureOpenAIResponses, ai.ApiGoogleGenerativeAI:
		return true
	}
	return false
}
