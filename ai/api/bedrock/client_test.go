package bedrock

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"

	"github.com/ihavespoons/tau/ai"
)

// THE POINT: region resolution decides which endpoint the request reaches, and
// every wrong answer here is a confusing failure somewhere else — a model that
// "does not exist", or a request billed against the wrong region.
func TestRegionResolutionPrecedence(t *testing.T) {
	const standard = "https://bedrock-runtime.us-east-1.amazonaws.com"
	const euStandard = "https://bedrock-runtime.eu-central-1.amazonaws.com"

	cases := []struct {
		name    string
		modelID string
		baseURL string
		opts    Options
		want    string
	}{
		{
			name:    "an ARN pins its own region over everything",
			modelID: "arn:aws:bedrock:ap-southeast-2:123:inference-profile/x",
			baseURL: standard,
			opts:    Options{Region: "us-west-2", StreamOptions: ai.StreamOptions{Env: map[string]string{"AWS_REGION": "eu-west-1"}}},
			want:    "ap-southeast-2",
		},
		{
			name:    "an explicit option beats the environment",
			modelID: "anthropic.claude-sonnet-5",
			baseURL: standard,
			opts:    Options{Region: "us-west-2", StreamOptions: ai.StreamOptions{Env: map[string]string{"AWS_REGION": "eu-west-1"}}},
			want:    "us-west-2",
		},
		{
			name:    "AWS_REGION beats AWS_DEFAULT_REGION",
			modelID: "anthropic.claude-sonnet-5",
			baseURL: standard,
			opts: Options{StreamOptions: ai.StreamOptions{Env: map[string]string{
				"AWS_REGION": "eu-west-1", "AWS_DEFAULT_REGION": "us-west-1",
			}}},
			want: "eu-west-1",
		},
		{
			name:    "AWS_DEFAULT_REGION is used when AWS_REGION is unset",
			modelID: "anthropic.claude-sonnet-5",
			baseURL: standard,
			opts:    Options{StreamOptions: ai.StreamOptions{Env: map[string]string{"AWS_DEFAULT_REGION": "us-west-1"}}},
			want:    "us-west-1",
		},
		{
			name:    "the catalog endpoint supplies the region when nothing else does",
			modelID: "eu.anthropic.claude-sonnet-5",
			baseURL: euStandard,
			opts:    Options{StreamOptions: ai.StreamOptions{Env: map[string]string{}}},
			want:    "eu-central-1",
		},
		{
			name:    "a custom endpoint falls back to the default region",
			modelID: "anthropic.claude-sonnet-5",
			baseURL: "https://bedrock.internal.example.com",
			opts:    Options{StreamOptions: ai.StreamOptions{Env: map[string]string{}}},
			want:    defaultRegion,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_PROFILE", "")
			model := &ai.Model{ID: tc.modelID, BaseURL: tc.baseURL}
			opts := tc.opts
			if got := resolveRegion(model, &opts); got != tc.want {
				t.Errorf("region %q, want %q", got, tc.want)
			}
		})
	}
}

// THE POINT: a catalog base URL is a default, not a decision. Pinning it over a
// user's configured region silently sends every request to us-east-1 — but a
// custom endpoint (a VPC endpoint, a gateway, a test server) must always win,
// or those setups cannot be reached at all.
func TestEndpointPinning(t *testing.T) {
	const standard = "https://bedrock-runtime.us-east-1.amazonaws.com"
	const custom = "https://gateway.internal.example.com"

	if !useExplicitEndpoint(custom, "eu-west-1", "work") {
		t.Error("a custom endpoint must be honoured even with a region and profile configured")
	}
	if !useExplicitEndpoint(standard, "", "") {
		t.Error("a standard endpoint should be pinned when nothing else is configured")
	}
	if useExplicitEndpoint(standard, "eu-west-1", "") {
		t.Error("a configured region must not be overridden by the catalog endpoint")
	}
	if useExplicitEndpoint(standard, "", "work") {
		t.Error("an ambient profile must not be overridden by the catalog endpoint")
	}
}

func TestStandardEndpointRegionParsing(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://bedrock-runtime.us-east-1.amazonaws.com", "us-east-1"},
		{"https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com", "us-gov-west-1"},
		{"https://bedrock-runtime.cn-north-1.amazonaws.com.cn", "cn-north-1"},
		{"https://gateway.example.com", ""},
		{"https://bedrock-runtime.us-east-1.amazonaws.com.evil.com", ""},
		{"", ""},
		{"not a url", ""},
	}
	for _, tc := range cases {
		if got := standardEndpointRegion(tc.url); got != tc.want {
			t.Errorf("standardEndpointRegion(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// A profile a user configured in their own shell must not be overridden by the
// catalog's baked-in default region.
func TestAnAmbientProfileSuppressesTheDefaultRegion(t *testing.T) {
	t.Setenv("AWS_PROFILE", "work")
	model := &ai.Model{ID: "anthropic.claude-sonnet-5", BaseURL: "https://bedrock.internal.example.com"}
	if got := resolveRegion(model, &Options{StreamOptions: ai.StreamOptions{Env: map[string]string{}}}); got != "" {
		t.Errorf("region %q — a profile's own region must be allowed to apply", got)
	}
}

// THE POINT: scoped environment values come from models.json and must reach the
// credential resolution, or a per-provider AWS account silently uses the wrong
// one — or none.
func TestStaticCredentialsComeFromTheScopedEnvironment(t *testing.T) {
	if got := staticCredentials(map[string]string{}); got != nil {
		t.Error("no keys must mean no static credentials, so the AWS chain runs")
	}
	if got := staticCredentials(map[string]string{"AWS_ACCESS_KEY_ID": "id"}); got != nil {
		t.Error("a half-configured pair must not produce credentials")
	}

	creds := staticCredentials(map[string]string{
		"AWS_ACCESS_KEY_ID": "id", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_SESSION_TOKEN": "token",
	})
	if creds == nil {
		t.Fatal("a complete pair must produce credentials")
	}
	resolved, err := creds.Retrieve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AccessKeyID != "id" || resolved.SecretAccessKey != "secret" || resolved.SessionToken != "token" {
		t.Errorf("credentials %+v", resolved)
	}
}

// A bearer token is a Bedrock API key; it replaces SigV4 entirely.
func TestBearerTokenPrecedence(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"explicit token wins", Options{BearerToken: "explicit", StreamOptions: ai.StreamOptions{APIKey: "key"}}, "explicit"},
		{"a resolved api key is used as the token", Options{StreamOptions: ai.StreamOptions{APIKey: "key"}}, "key"},
		{"the environment supplies it last", Options{StreamOptions: ai.StreamOptions{
			Env: map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "from-env"},
		}}, "from-env"},
		{"nothing configured", Options{StreamOptions: ai.StreamOptions{Env: map[string]string{}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if got := bearerToken(&opts); got != tc.want {
				t.Errorf("bearerToken = %q, want %q", got, tc.want)
			}
		})
	}
}

// THE POINT: a proxy in front of Bedrock does its own authentication and
// rejects a signed request, but the SDK still has to compute a signature.
// Dummy credentials are what let that request be built at all.
func TestSkipAuthUsesDummyCredentialsAndSuppressesTheBearerToken(t *testing.T) {
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))

	_, msg := collect(t, Stream(t.Context(), testModel(url), userContext("hi"), &Options{
		BearerToken: "should-be-ignored",
		StreamOptions: ai.StreamOptions{Env: map[string]string{
			"AWS_BEDROCK_SKIP_AUTH": "1", "AWS_REGION": "us-east-1",
		}},
	}))
	if msg.StopReason == ai.StopError {
		t.Fatalf("request failed: %s", msg.ErrorMessage)
	}

	auth := cap.Headers.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("want a signed request with dummy credentials, got %q", auth)
	}
	if strings.Contains(auth, "should-be-ignored") {
		t.Errorf("the bearer token was used despite skip-auth: %q", auth)
	}
}

// Caller headers ride on the request. They are attached before signing, so the
// signature covers them and the service does not reject the request.
func TestCustomHeadersReachTheRequestSigned(t *testing.T) {
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))

	model := testModel(url)
	model.Headers = map[string]string{"X-Tenant": "acme"}
	override := "gateway-value"
	_, msg := collect(t, Stream(t.Context(), model, userContext("hi"), &Options{
		StreamOptions: ai.StreamOptions{Env: testEnv(), Headers: map[string]*string{"X-Route": &override}},
	}))
	if msg.StopReason == ai.StopError {
		t.Fatalf("request failed: %s", msg.ErrorMessage)
	}

	if got := cap.Headers.Get("X-Tenant"); got != "acme" {
		t.Errorf("model header: %q", got)
	}
	if got := cap.Headers.Get("X-Route"); got != "gateway-value" {
		t.Errorf("option header: %q", got)
	}
	// A signed request whose signature does not cover an injected header would
	// be rejected by AWS, so the header must be inside SignedHeaders.
	auth := cap.Headers.Get("Authorization")
	if !strings.Contains(strings.ToLower(auth), "x-route") {
		t.Errorf("injected headers are not covered by the signature: %q", auth)
	}
}

// THE POINT: overwriting a signing header invalidates the signature, and the
// failure surfaces as an opaque 403 rather than anything pointing at the header.
func TestReservedHeadersCannotBeOverwritten(t *testing.T) {
	for _, name := range []string{"Authorization", "authorization", "Host", "X-Amz-Date", "x-amz-content-sha256"} {
		if !isReservedHeader(name) {
			t.Errorf("%q must be reserved", name)
		}
	}
	for _, name := range []string{"X-Tenant", "Content-Type", "X-Amzn-Trace-Id"} {
		if isReservedHeader(name) {
			t.Errorf("%q must not be reserved", name)
		}
	}

	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))
	// An x-amz-* header is the observable case. Authorization would prove
	// nothing here — the signer runs after this middleware and overwrites it
	// either way — whereas an x-amz-* header the caller injects survives to the
	// wire and is covered by the signature, so AWS recomputes it and rejects
	// the request.
	forged := "forged-checksum"
	_, msg := collect(t, Stream(t.Context(), testModel(url), userContext("hi"), &Options{
		StreamOptions: ai.StreamOptions{Env: testEnv(), Headers: map[string]*string{
			"X-Amz-Content-Sha256": &forged,
		}},
	}))
	if msg.StopReason == ai.StopError {
		t.Fatalf("request failed: %s", msg.ErrorMessage)
	}
	if got := cap.Headers.Get("X-Amz-Content-Sha256"); got == forged {
		t.Error("a caller header overwrote a signing header; AWS would reject this request")
	}
}

// A scoped proxy setting has to be honoured; the SDK only reads the process
// environment on its own.
func TestScopedProxySettingsAreHonoured(t *testing.T) {
	if proxyFunc(nil) != nil {
		t.Error("no scoped environment must leave the SDK's own proxy handling alone")
	}
	if proxyFunc(map[string]string{"AWS_REGION": "us-east-1"}) != nil {
		t.Error("a scope with no proxy settings must not install a resolver")
	}
	if proxyFunc(map[string]string{"https_proxy": "http://proxy.example.com:3128"}) == nil {
		t.Error("a scoped https_proxy must install a resolver")
	}
	if proxyFunc(map[string]string{"HTTPS_PROXY": "http://proxy.example.com:3128"}) == nil {
		t.Error("the uppercase spelling must work too")
	}
}

// THE POINT: the prefixes are matched by the retry logic above this layer.
// Using the raw SDK exception names would silently stop throttled and
// server-error requests from being retried at all.
func TestErrorPrefixesArePreserved(t *testing.T) {
	cases := []struct {
		code, want string
	}{
		{"ThrottlingException", "Throttling error"},
		{"InternalServerException", "Internal server error"},
		{"ModelStreamErrorException", "Model stream error"},
		{"ValidationException", "Validation error"},
		{"ServiceUnavailableException", "Service unavailable"},
		{"AccessDeniedException", "AccessDeniedException"},
	}
	for _, tc := range cases {
		err := &smithy.GenericAPIError{Code: tc.code, Message: "something happened"}
		got := formatError(err)
		if !strings.HasPrefix(got, tc.want+": ") {
			t.Errorf("formatError(%s) = %q, want prefix %q", tc.code, got, tc.want)
		}
	}
}

// A retention-mode rejection is opaque unless the message says where to look.
func TestDataRetentionErrorsCarryTheDocsLink(t *testing.T) {
	err := &smithy.GenericAPIError{
		Code:    "ValidationException",
		Message: "data retention mode 'default' is not available for this model",
	}
	got := formatError(err)
	if !strings.Contains(got, dataRetentionDocsURL) {
		t.Errorf("formatError = %q, want the docs link", got)
	}
}

// A plain error passes through unchanged; nothing is invented around it.
func TestAPlainErrorIsNotDecorated(t *testing.T) {
	if got := formatError(errors.New("dial tcp: connection refused")); got != "dial tcp: connection refused" {
		t.Errorf("formatError = %q", got)
	}
	if got := formatError(nil); got != "" {
		t.Errorf("formatError(nil) = %q", got)
	}
}

func TestStopReasonMapping(t *testing.T) {
	cases := []struct {
		in   types.StopReason
		want ai.StopReason
	}{
		{types.StopReasonEndTurn, ai.StopStop},
		{types.StopReasonStopSequence, ai.StopStop},
		{types.StopReasonMaxTokens, ai.StopLength},
		{types.StopReasonModelContextWindowExceeded, ai.StopLength},
		{types.StopReasonToolUse, ai.StopToolUse},
		{types.StopReason("guardrail_intervened"), ai.StopError},
	}
	for _, tc := range cases {
		got, _ := mapStopReason(tc.in)
		if got != tc.want {
			t.Errorf("mapStopReason(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// An unmapped reason must carry its name, or the user sees a bare "error".
	if _, message := mapStopReason(types.StopReason("guardrail_intervened")); message != "guardrail_intervened" {
		t.Errorf("error message %q", message)
	}
}
