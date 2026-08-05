package bedrock

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/auth/bearer"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"golang.org/x/net/http/httpproxy"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// Bedrock is the one wire tau does not speak directly. Requests are SigV4
// signed and responses arrive as AWS EventStream binary frames, and the
// credential chain reaches shared config files, SSO caches and IMDS. Rebuilding
// that would be a large surface to get subtly wrong, so this file assembles an
// aws-sdk-go-v2 client and the rest of the package treats it as the transport.

// defaultRegion is where Bedrock is reached when nothing else says otherwise.
const defaultRegion = "us-east-1"

// standardEndpointPattern matches the AWS-operated Bedrock runtime hosts. A
// base URL that does not match is a custom endpoint — a VPC endpoint, a
// gateway, a proxy, or a test server — and is always honoured verbatim.
var standardEndpointPattern = regexp.MustCompile(`^bedrock-runtime(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)

// arnRegionPattern pulls the region out of an inference-profile ARN used as a
// model id.
var arnRegionPattern = regexp.MustCompile(`^arn:aws(-[a-z0-9-]+)?:bedrock:([a-z0-9-]+):`)

// standardEndpointRegion returns the region named by a standard Bedrock host,
// or "" when the URL is a custom endpoint.
func standardEndpointRegion(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	m := standardEndpointPattern.FindStringSubmatch(strings.ToLower(u.Hostname()))
	if m == nil {
		return ""
	}
	return m[2]
}

// configuredRegion is the region the caller asked for, ignoring defaults.
func configuredRegion(opts *Options) string {
	if opts.Region != "" {
		return opts.Region
	}
	if v := apishared.EnvValue(opts.Env, "AWS_REGION"); v != "" {
		return v
	}
	return apishared.EnvValue(opts.Env, "AWS_DEFAULT_REGION")
}

// ambientProfile reports whether the user's own shell configures an AWS
// profile.
//
// This deliberately reads the process environment only, never the scoped
// override map. It is not asking "which profile should we use" but "has the
// user already set up AWS on this machine" — and if they have, the catalog's
// built-in us-east-1 default must not override their configuration.
func ambientProfile() string { return os.Getenv("AWS_PROFILE") }

// useExplicitEndpoint reports whether model.BaseURL should be sent as the
// endpoint rather than letting the SDK derive one from the region.
//
// A custom endpoint is always honoured. A standard one is pinned only when the
// user has configured neither a region nor a profile, because the catalog's
// baked-in us-east-1 URL would otherwise silently override an AWS_REGION the
// user set for their whole shell.
func useExplicitEndpoint(baseURL, region, profile string) bool {
	if standardEndpointRegion(baseURL) == "" {
		return true
	}
	return region == "" && profile == ""
}

// resolveRegion ports Pi's precedence: a region embedded in an inference-profile
// ARN beats everything, because that ARN is only valid in its own region.
func resolveRegion(model *ai.Model, opts *Options) string {
	if m := arnRegionPattern.FindStringSubmatch(model.ID); m != nil {
		return m[2]
	}
	if r := configuredRegion(opts); r != "" {
		return r
	}
	if useExplicitEndpoint(model.BaseURL, configuredRegion(opts), ambientProfile()) {
		if r := standardEndpointRegion(model.BaseURL); r != "" {
			return r
		}
	}
	if ambientProfile() != "" {
		// A profile can carry its own region; leaving this empty lets the
		// shared-config loader find it instead of forcing a default over it.
		return ""
	}
	return defaultRegion
}

// staticCredentials returns credentials configured by environment variables, or
// nil to fall through to the SDK's own chain.
func staticCredentials(env map[string]string) aws.CredentialsProvider {
	id := apishared.EnvValue(env, "AWS_ACCESS_KEY_ID")
	secret := apishared.EnvValue(env, "AWS_SECRET_ACCESS_KEY")
	if id == "" || secret == "" {
		return nil
	}
	return credentials.NewStaticCredentialsProvider(id, secret, apishared.EnvValue(env, "AWS_SESSION_TOKEN"))
}

// bearerToken resolves a Bedrock API key, which replaces SigV4 signing with an
// Authorization: Bearer header.
func bearerToken(opts *Options) string {
	if opts.BearerToken != "" {
		return opts.BearerToken
	}
	if opts.APIKey != "" {
		return opts.APIKey
	}
	return apishared.EnvValue(opts.Env, "AWS_BEARER_TOKEN_BEDROCK")
}

func skipAuth(env map[string]string) bool {
	return apishared.EnvValue(env, "AWS_BEDROCK_SKIP_AUTH") == "1"
}

// buildClient assembles the Bedrock runtime client for one request.
func buildClient(ctx context.Context, model *ai.Model, opts *Options) (*bedrockruntime.Client, error) {
	region := resolveRegion(model, opts)
	profile := opts.Profile
	if profile == "" {
		profile = apishared.EnvValue(opts.Env, "AWS_PROFILE")
	}

	var creds aws.CredentialsProvider
	switch {
	case skipAuth(opts.Env):
		// Some gateways in front of Bedrock do their own authentication and
		// reject a signed request. The signature still has to be computed, so
		// feed it credentials that exist but mean nothing.
		creds = credentials.NewStaticCredentialsProvider("dummy-access-key", "dummy-secret-key", "")
	default:
		creds = staticCredentials(opts.Env)
	}

	token := bearerToken(opts)
	useBearer := token != "" && !skipAuth(opts.Env)

	awsOpts := bedrockruntime.Options{
		Region:     region,
		HTTPClient: httpClient(model.BaseURL, opts),
		APIOptions: []func(*middleware.Stack) error{},
	}

	if useExplicitEndpoint(model.BaseURL, configuredRegion(opts), ambientProfile()) && model.BaseURL != "" {
		awsOpts.BaseEndpoint = aws.String(model.BaseURL)
	}

	switch {
	case useBearer:
		// A bearer token is complete on its own: no credential chain is
		// consulted, so nothing here can reach the filesystem or IMDS.
		awsOpts.BearerAuthTokenProvider = bearer.TokenProviderFunc(func(context.Context) (bearer.Token, error) {
			return bearer.Token{Value: token}, nil
		})
		awsOpts.AuthSchemePreference = []string{"httpBearerAuth"}
	case creds != nil:
		awsOpts.Credentials = creds
	default:
		// Nothing explicit was configured, so fall back to the full AWS chain:
		// shared config and credentials files, SSO, container and instance
		// roles. This is the only path that touches the filesystem or network
		// before the request, and it is the one Bedrock users expect.
		var loadOpts []func(*config.LoadOptions) error
		if region != "" {
			loadOpts = append(loadOpts, config.WithRegion(region))
		}
		if profile != "" {
			loadOpts = append(loadOpts, config.WithSharedConfigProfile(profile))
		}
		cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
		if err != nil {
			return nil, fmt.Errorf("resolving AWS credentials: %w", err)
		}
		awsOpts.Credentials = cfg.Credentials
		if awsOpts.Region == "" {
			awsOpts.Region = cfg.Region
		}
	}

	if awsOpts.Region == "" {
		awsOpts.Region = defaultRegion
	}

	if headers := customHeaders(model, opts); len(headers) > 0 {
		awsOpts.APIOptions = append(awsOpts.APIOptions, customHeadersMiddleware(headers))
	}

	return bedrockruntime.New(awsOpts), nil
}

// httpClient builds the transport, honouring a proxy configured for this
// provider and the HTTP/1.1 escape hatch some custom endpoints need.
func httpClient(baseURL string, opts *Options) *awshttp.BuildableClient {
	client := awshttp.NewBuildableClient()
	forceHTTP1 := apishared.EnvValue(opts.Env, "AWS_BEDROCK_FORCE_HTTP1") == "1"
	proxy := proxyFunc(opts.Env)

	if !forceHTTP1 && proxy == nil {
		return client
	}
	return client.WithTransportOptions(func(t *http.Transport) {
		if proxy != nil {
			t.Proxy = proxy
		}
		if forceHTTP1 {
			// A non-nil empty map is how net/http is told not to negotiate h2.
			t.ForceAttemptHTTP2 = false
			t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
	})
}

// proxyFunc returns a proxy resolver built from the provider's scoped
// environment, or nil to leave the SDK's own HTTP_PROXY handling in place.
//
// The SDK already reads the process environment, so this only has to cover the
// case where a models.json entry sets proxy variables for one provider.
func proxyFunc(env map[string]string) func(*http.Request) (*url.URL, error) {
	if len(env) == 0 {
		return nil
	}
	pick := func(name string) string {
		if v, ok := env[strings.ToLower(name)]; ok && v != "" {
			return v
		}
		if v, ok := env[strings.ToUpper(name)]; ok && v != "" {
			return v
		}
		return ""
	}
	cfg := httpproxy.Config{
		HTTPProxy:  pick("http_proxy"),
		HTTPSProxy: pick("https_proxy"),
		NoProxy:    pick("no_proxy"),
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		return nil
	}
	// Fall back to the process environment for anything the scope left unset,
	// so a scoped https_proxy does not silently discard an ambient no_proxy.
	if cfg.NoProxy == "" {
		cfg.NoProxy = firstNonEmpty(os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
	}
	resolve := cfg.ProxyFunc()
	return func(req *http.Request) (*url.URL, error) { return resolve(req.URL) }
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// customHeaders merges the model's static headers with per-request overrides.
func customHeaders(model *ai.Model, opts *Options) map[string]string {
	out := map[string]string{}
	for k, v := range model.Headers {
		out[k] = v
	}
	for k, v := range opts.Headers {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = *v
	}
	return out
}

// reservedHeaders are owned by the signing process. `host` and every `x-amz-*`
// header take part in the SigV4 canonical request, and `authorization` is the
// signature itself — overwriting any of them invalidates the request.
var reservedHeaders = map[string]bool{"authorization": true, "host": true}

func isReservedHeader(key string) bool {
	lower := strings.ToLower(key)
	return strings.HasPrefix(lower, "x-amz-") || reservedHeaders[lower]
}

// customHeadersMiddleware attaches caller headers after serialization but
// before signing, so the signature covers them.
func customHeadersMiddleware(headers map[string]string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Build.Add(middleware.BuildMiddlewareFunc("tauCustomHeaders",
			func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (
				middleware.BuildOutput, middleware.Metadata, error,
			) {
				if req, ok := in.Request.(*smithyhttp.Request); ok {
					for k, v := range headers {
						if isReservedHeader(k) {
							continue
						}
						req.Header.Set(k, v)
					}
				}
				return next.HandleBuild(ctx, in)
			}), middleware.After)
	}
}
