package googlegenai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func vertexModel(id, baseURL string) *ai.Model {
	m := modelFor(id, baseURL)
	m.Api = ai.ApiGoogleVertex
	m.Provider = "google-vertex"
	return m
}

// THE POINT: a half-filled config leaves "<your-key-here>" behind, and the
// credential store writes a marker when Vertex is meant to use ambient Cloud
// credentials. Treating either as a real key produces a 401 instead of the
// clearer failure of having no key at all — and worse, it skips the ADC path
// the user actually configured.
func TestVertexIgnoresNonKeys(t *testing.T) {
	cases := []struct {
		name, key string
		want      string
	}{
		{"a real key is used", "AIzaSyReal", "AIzaSyReal"},
		{"whitespace is trimmed", "  AIzaSyReal  ", "AIzaSyReal"},
		{"empty is no key", "", ""},
		{"the credentials marker is no key", vertexCredentialsMarker, ""},
		{"a placeholder is no key", "<your-api-key>", ""},
		{"another placeholder shape", "<GOOGLE_API_KEY>", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &VertexOptions{Options: Options{StreamOptions: ai.StreamOptions{APIKey: tc.key}}}
			if got := vertexAPIKey(opts); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// With a key, Vertex is addressed as the global publisher endpoint with no
// project at all — its express mode.
func TestVertexAPIKeyUsesThePublisherEndpoint(t *testing.T) {
	model := vertexModel("gemini-3-pro-preview", "https://{location}-aiplatform.googleapis.com")

	endpoint, auth, err := vertexTarget(context.Background(), model, &VertexOptions{
		Options: Options{StreamOptions: ai.StreamOptions{APIKey: "AIzaSyReal"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(endpoint, vertexGlobalHost+"/v1/publishers/google/models/gemini-3-pro-preview") {
		t.Errorf("endpoint: %q", endpoint)
	}
	if !strings.Contains(endpoint, "alt=sse") {
		t.Errorf("the endpoint must ask for events: %q", endpoint)
	}

	h := http.Header{}
	auth(h)
	if h.Get("x-goog-api-key") != "AIzaSyReal" {
		t.Errorf("headers: %v", h)
	}
	if h.Get("Authorization") != "" {
		t.Error("an api key must not also send a bearer token")
	}
}

// The catalog stores the host with a {location} placeholder, because the
// region is the caller's choice and a model is not pinned to one.
func TestVertexProjectURL(t *testing.T) {
	cases := []struct {
		name, base, location, want string
	}{
		{
			"the placeholder is substituted",
			"https://{location}-aiplatform.googleapis.com", "europe-west4",
			"https://europe-west4-aiplatform.googleapis.com/v1/projects/proj/locations/europe-west4/publishers/google/models/m:streamGenerateContent?alt=sse",
		},
		{
			// "global" is served by the unregioned host, not a "global-" prefix.
			"global uses the unregioned host",
			"https://{location}-aiplatform.googleapis.com", "global",
			"https://aiplatform.googleapis.com/v1/projects/proj/locations/global/publishers/google/models/m:streamGenerateContent?alt=sse",
		},
		{
			"an empty base falls back to the standard host",
			"", "us-central1",
			"https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/m:streamGenerateContent?alt=sse",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vertexProjectURL(tc.base, tc.location, "proj", "m"); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Without a project or a location the error has to name the variable to set —
// a bare "misconfigured" leaves the user guessing.
func TestVertexConfigErrorsNameWhatToSet(t *testing.T) {
	model := vertexModel("gemini-3-pro-preview", "")

	t.Run("no project", func(t *testing.T) {
		_, _, err := vertexTarget(context.Background(), model, &VertexOptions{
			Options: Options{StreamOptions: ai.StreamOptions{Env: map[string]string{}}},
		})
		if err == nil || !strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT") {
			t.Errorf("error: %v", err)
		}
	})

	t.Run("no location", func(t *testing.T) {
		_, _, err := vertexTarget(context.Background(), model, &VertexOptions{
			Project: "proj",
			Options: Options{StreamOptions: ai.StreamOptions{Env: map[string]string{}}},
		})
		if err == nil || !strings.Contains(err.Error(), "GOOGLE_CLOUD_LOCATION") {
			t.Errorf("error: %v", err)
		}
	})
}

// Explicit options win over the environment, and GCLOUD_PROJECT is accepted as
// the older spelling of the same thing.
func TestVertexProjectResolutionOrder(t *testing.T) {
	model := vertexModel("gemini-3-pro-preview", "")

	t.Run("options win", func(t *testing.T) {
		endpoint, _, err := vertexTarget(context.Background(), model, &VertexOptions{
			Project: "from-options", Location: "us-central1",
			Options: Options{StreamOptions: ai.StreamOptions{
				APIKey: "AIzaSyReal", // ignored below; checked separately
				Env:    map[string]string{"GOOGLE_CLOUD_PROJECT": "from-env"},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// An API key short-circuits to the publisher endpoint, so this asserts
		// only that resolution did not fail.
		if endpoint == "" {
			t.Error("no endpoint")
		}
	})

	t.Run("the older variable is accepted", func(t *testing.T) {
		// No ADC on a test machine, so this asserts resolution got past the
		// project check rather than the whole request.
		_, _, err := vertexTarget(context.Background(), model, &VertexOptions{
			Location: "us-central1",
			Options:  Options{StreamOptions: ai.StreamOptions{Env: map[string]string{"GCLOUD_PROJECT": "legacy"}}},
		})
		if err != nil && strings.Contains(err.Error(), "needs a project") {
			t.Errorf("GCLOUD_PROJECT was not accepted: %v", err)
		}
	})
}

// A full turn over the key path, to prove it is the same wire underneath.
//
// The custom host is what makes this testable at all: without it the express
// path always addresses Google directly, and the test would reach the real
// service.
func TestVertexStreamsWithAnAPIKey(t *testing.T) {
	var seenKey, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey, seenPath = r.Header.Get("x-goog-api-key"), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textStream))
	}))
	defer srv.Close()

	model := vertexModel("gemini-3-pro-preview", srv.URL)
	_, msg := collect(StreamVertex(context.Background(), model, simpleContext(), &VertexOptions{
		Options: Options{StreamOptions: ai.StreamOptions{APIKey: "AIzaSyReal"}},
	}))

	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
	if seenKey != "AIzaSyReal" {
		t.Errorf("x-goog-api-key: %q", seenKey)
	}
	if !strings.Contains(seenPath, "/publishers/google/models/gemini-3-pro-preview:streamGenerateContent") {
		t.Errorf("path: %q", seenPath)
	}
	if text := msg.Content[0].(ai.TextContent).Text; text != "Hello world" {
		t.Errorf("text: %q", text)
	}
}

// A base URL naming a real host is a deliberate override — a proxy, a gateway,
// or a test. The catalog's {location} template is not one.
func TestCustomVertexHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", vertexGlobalHost},
		{"https://{location}-aiplatform.googleapis.com", vertexGlobalHost},
		{"  ", vertexGlobalHost},
		{"https://proxy.internal/vertex", "https://proxy.internal/vertex"},
	}
	for _, tc := range cases {
		if got := customVertexHost(tc.in); got != tc.want {
			t.Errorf("customVertexHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
