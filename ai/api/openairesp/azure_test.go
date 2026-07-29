package openairesp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func azureModel(baseURL string) *ai.Model {
	m := modelFor("azure-openai-responses", baseURL)
	m.Api = ai.ApiAzureOpenAIResponses
	return m
}

// THE POINT: the Azure portal shows a bare resource endpoint and the docs show
// the full path to the responses endpoint. Neither is what the request needs,
// and a user who pastes either should get a working session rather than a 404
// they have no way to interpret.
func TestAzureBaseURLNormalization(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"a bare resource endpoint", "https://my-res.openai.azure.com",
			"https://my-res.openai.azure.com/openai/v1"},
		{"a trailing slash", "https://my-res.openai.azure.com/",
			"https://my-res.openai.azure.com/openai/v1"},
		{"the /openai prefix alone", "https://my-res.openai.azure.com/openai",
			"https://my-res.openai.azure.com/openai/v1"},
		{"the full documented path", "https://my-res.openai.azure.com/openai/v1/responses",
			"https://my-res.openai.azure.com/openai/v1"},
		{"already correct", "https://my-res.openai.azure.com/openai/v1",
			"https://my-res.openai.azure.com/openai/v1"},
		{"a cognitiveservices host", "https://my-res.cognitiveservices.azure.com",
			"https://my-res.cognitiveservices.azure.com/openai/v1"},
		{"an ai.azure.com host", "https://my-res.ai.azure.com",
			"https://my-res.ai.azure.com/openai/v1"},
		// A proxy in front of Azure is addressed exactly as given: rewriting
		// its path would send the request somewhere it does not serve.
		{"a gateway is left alone", "https://gateway.internal/azure-passthrough",
			"https://gateway.internal/azure-passthrough"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeAzureBaseURL(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAzureRejectsAnUnusableBaseURL(t *testing.T) {
	if _, err := normalizeAzureBaseURL("not a url"); err == nil {
		t.Error("a base URL with no host must be rejected, not sent")
	}
}

// The endpoint comes from whichever source the user configured, in a fixed
// order, and the api-version rides along.
func TestAzureEndpointResolution(t *testing.T) {
	model := azureModel("")

	t.Run("an explicit base URL wins", func(t *testing.T) {
		got, err := azureEndpoint(model, &AzureOptions{
			BaseURL:      "https://explicit.openai.azure.com",
			ResourceName: "ignored",
			Options:      Options{StreamOptions: ai.StreamOptions{Env: map[string]string{"AZURE_OPENAI_BASE_URL": "https://env.openai.azure.com"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "https://explicit.openai.azure.com/openai/v1/responses") {
			t.Errorf("endpoint: %q", got)
		}
	})

	t.Run("a resource name builds the endpoint", func(t *testing.T) {
		got, err := azureEndpoint(model, &AzureOptions{ResourceName: "my-res"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "https://my-res.openai.azure.com/openai/v1/responses") {
			t.Errorf("endpoint: %q", got)
		}
	})

	t.Run("the environment supplies both", func(t *testing.T) {
		got, err := azureEndpoint(model, &AzureOptions{Options: Options{
			StreamOptions: ai.StreamOptions{Env: map[string]string{
				"AZURE_OPENAI_RESOURCE_NAME": "env-res",
				"AZURE_OPENAI_API_VERSION":   "2025-04-01-preview",
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "env-res.openai.azure.com") {
			t.Errorf("endpoint: %q", got)
		}
		if !strings.Contains(got, "api-version=2025-04-01-preview") {
			t.Errorf("the api version was not applied: %q", got)
		}
	})

	t.Run("the default api version is applied", func(t *testing.T) {
		got, _ := azureEndpoint(model, &AzureOptions{ResourceName: "my-res"})
		if !strings.HasSuffix(got, "api-version="+azureAPIVersion) {
			t.Errorf("endpoint: %q", got)
		}
	})

	// With nothing configured the error has to name what to set. A bare
	// "missing endpoint" leaves the user guessing between three mechanisms.
	t.Run("no endpoint names the options", func(t *testing.T) {
		_, err := azureEndpoint(model, &AzureOptions{})
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_RESOURCE_NAME", "baseUrl"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error should mention %q: %v", want, err)
			}
		}
	})
}

// THE POINT: an organisation's deployment names rarely match the model ids,
// and requiring a models.json entry per deployment would be a lot of ceremony
// for a rename.
func TestAzureDeploymentNameResolution(t *testing.T) {
	model := azureModel("https://res.openai.azure.com")

	t.Run("defaults to the model id", func(t *testing.T) {
		if got := azureDeployment(model, &AzureOptions{}); got != model.ID {
			t.Errorf("deployment: %q", got)
		}
	})

	t.Run("an explicit name wins", func(t *testing.T) {
		if got := azureDeployment(model, &AzureOptions{DeploymentName: "chosen"}); got != "chosen" {
			t.Errorf("deployment: %q", got)
		}
	})

	t.Run("the map is consulted", func(t *testing.T) {
		opts := &AzureOptions{Options: Options{StreamOptions: ai.StreamOptions{Env: map[string]string{
			"AZURE_OPENAI_DEPLOYMENT_NAME_MAP": "other=nope, " + model.ID + " = my-deployment ,third=no",
		}}}}
		if got := azureDeployment(model, opts); got != "my-deployment" {
			t.Errorf("deployment: %q — whitespace around entries should be tolerated", got)
		}
	})

	t.Run("a map without this model falls through", func(t *testing.T) {
		opts := &AzureOptions{Options: Options{StreamOptions: ai.StreamOptions{Env: map[string]string{
			"AZURE_OPENAI_DEPLOYMENT_NAME_MAP": "something-else=other",
		}}}}
		if got := azureDeployment(model, opts); got != model.ID {
			t.Errorf("deployment: %q", got)
		}
	})
}

// A full turn: the deployment name is what goes in the body, the api-key
// header is what authenticates, and the retention controls Azure does not
// serve are withheld.
func TestAzureRequestShape(t *testing.T) {
	var (
		seenPath  string
		seenQuery string
		seenAuth  string
		seenKey   string
		seenBody  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenQuery = r.URL.Path, r.URL.RawQuery
		seenAuth, seenKey = r.Header.Get("Authorization"), r.Header.Get("api-key")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		seenBody = string(buf)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textBody))
	}))
	defer srv.Close()

	model := azureModel(srv.URL)
	_, msg := collect(StreamAzure(context.Background(), model, simpleContext(), &AzureOptions{
		Options:        Options{StreamOptions: ai.StreamOptions{APIKey: "azure-key", SessionID: "s1", CacheRetention: ai.CacheLong}},
		DeploymentName: "my-deployment",
	}))

	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
	if !strings.HasSuffix(seenPath, "/responses") {
		t.Errorf("path: %q", seenPath)
	}
	if !strings.Contains(seenQuery, "api-version=") {
		t.Errorf("query: %q", seenQuery)
	}
	// Azure authenticates with its own header; a bearer token is ignored.
	if seenKey != "azure-key" {
		t.Errorf("api-key header: %q", seenKey)
	}
	if seenAuth != "" {
		t.Errorf("a bearer token was sent to azure: %q", seenAuth)
	}
	if !strings.Contains(seenBody, `"model":"my-deployment"`) {
		t.Errorf("the deployment name did not reach the body: %s", seenBody)
	}
	if strings.Contains(seenBody, "prompt_cache_retention") {
		t.Errorf("azure does not serve retention controls: %s", seenBody)
	}
}

// Strict tool schemas are a property of the Azure surface rather than of a
// model, so they are on unless a model says otherwise.
func TestAzureEnablesStrictToolsByDefault(t *testing.T) {
	model := azureModel("https://res.openai.azure.com")
	if !resolveAzureCompat(model).SupportsStrictMode {
		t.Error("azure should declare strict tools")
	}

	model.Compat = &ai.CompatFlags{SupportsStrictMode: boolptr(false)}
	if resolveAzureCompat(model).SupportsStrictMode {
		t.Error("a model saying otherwise must win")
	}
}

func TestAzureRequiresAKey(t *testing.T) {
	_, msg := collect(StreamAzure(context.Background(),
		azureModel("https://res.openai.azure.com"), simpleContext(), &AzureOptions{}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "API key") {
		t.Errorf("error: %q", msg.ErrorMessage)
	}
}
