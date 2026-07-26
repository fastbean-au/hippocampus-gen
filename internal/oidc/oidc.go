// Package oidc provides the bearer-token auth the generators use against a Hippocampus service that
// requires it. It offers a static token source and an OIDC client-credentials source (a machine-to-
// machine grant, refreshed automatically before it expires), plus a gRPC unary client interceptor
// that stamps the current token onto every outgoing RPC as "authorization: Bearer <token>" metadata
// - the form the service's auth interceptor reads.
package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// refreshSkew is how far ahead of a token's expiry the client-credentials source refreshes it, so a
// long-running RPC never travels with an about-to-expire token.
const refreshSkew = 30 * time.Second

// defaultTokenTTL is assumed when a token response omits expires_in, so the source still refreshes
// rather than caching a token forever.
const defaultTokenTTL = 5 * time.Minute

// httpTimeout bounds a single call to the discovery or token endpoint.
const httpTimeout = 15 * time.Second

// bodyLimit caps how much of a token/discovery response is read, so a misbehaving endpoint cannot
// stream an unbounded body into memory.
const bodyLimit = 1 << 20

// Source yields a currently-valid bearer token. Token may block to fetch or refresh one.
type Source interface {
	Token(ctx context.Context) (string, error)
}

// StaticSource returns a fixed token supplied out of band (the --token flag). Use NewStaticSource.
type StaticSource struct {
	token string
}

// NewStaticSource wraps a fixed token as a Source.
func NewStaticSource(token string) StaticSource {
	return StaticSource{token: token}
}

// Token returns the fixed token unchanged.
func (s StaticSource) Token(_ context.Context) (string, error) {
	return s.token, nil
}

// ClientCredentialsConfig configures a machine-to-machine OIDC grant. Either TokenURL (the token
// endpoint directly) or Issuer (from which the token endpoint is discovered via
// <issuer>/.well-known/openid-configuration) must be set. Audience is required by providers whose
// access token is opaque without one (Auth0's API identifier); Keycloak ignores it. HTTPClient is
// optional and defaults to one with a sane timeout.
type ClientCredentialsConfig struct {
	Issuer       string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
	Audience     string
	HTTPClient   *http.Client
}

// ClientCredentialsSource fetches an access token via the OIDC client-credentials grant and caches
// it until shortly before it expires, refreshing on demand. It is safe for concurrent use.
type ClientCredentialsSource struct {
	cfg        ClientCredentialsConfig
	httpClient *http.Client
	tokenURL   string

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewClientCredentialsSource validates the config, resolves the token endpoint (discovering it from
// the issuer when TokenURL is not given), and returns a ready source. It does not fetch a token yet;
// the first Token call does.
func NewClientCredentialsSource(ctx context.Context, cfg ClientCredentialsConfig) (*ClientCredentialsSource, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {

		return nil, fmt.Errorf("client-credentials auth requires both a client id and secret")
	}

	if cfg.TokenURL == "" && cfg.Issuer == "" {

		return nil, fmt.Errorf("client-credentials auth requires either a token url or an issuer")
	}

	httpClient := cfg.HTTPClient

	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}

	tokenURL := cfg.TokenURL

	if tokenURL == "" {
		discovered, err := discoverTokenURL(ctx, httpClient, cfg.Issuer)
		if err != nil {

			return nil, err
		}

		tokenURL = discovered
	}

	return &ClientCredentialsSource{cfg: cfg, httpClient: httpClient, tokenURL: tokenURL}, nil
}

// Token returns a cached token when one is still comfortably valid, otherwise fetches a fresh one.
func (s *ClientCredentialsSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && time.Now().Add(refreshSkew).Before(s.expiry) {

		return s.token, nil
	}

	if err := s.fetch(ctx); err != nil {

		return "", err
	}

	return s.token, nil
}

// fetch posts the client-credentials grant to the token endpoint and stores the resulting token and
// its expiry. The caller holds s.mu.
func (s *ClientCredentialsSource) fetch(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)

	if s.cfg.Scope != "" {
		form.Set("scope", s.cfg.Scope)
	}

	// Auth0 returns an opaque token unless an API audience is requested; Keycloak ignores it.
	if s.cfg.Audience != "" {
		form.Set("audience", s.cfg.Audience)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {

		return fmt.Errorf("building token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {

		return fmt.Errorf("requesting token: %w", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {

		return fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {

		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}

	if err := json.Unmarshal(body, &tr); err != nil {

		return fmt.Errorf("decoding token response: %w", err)
	}

	if tr.AccessToken == "" {

		return fmt.Errorf("token response carried no access_token")
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second

	if ttl <= 0 {
		ttl = defaultTokenTTL
	}

	s.token = tr.AccessToken
	s.expiry = time.Now().Add(ttl)

	return nil
}

// discoverTokenURL reads the provider's OIDC metadata and returns its token endpoint.
func discoverTokenURL(ctx context.Context, httpClient *http.Client, issuer string) (string, error) {
	metaURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {

		return "", fmt.Errorf("building discovery request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {

		return "", fmt.Errorf("fetching OIDC discovery document: %w", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {

		return "", fmt.Errorf("reading OIDC discovery document: %w", err)
	}

	if resp.StatusCode != http.StatusOK {

		return "", fmt.Errorf("OIDC discovery returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var meta struct {
		TokenEndpoint string `json:"token_endpoint"`
	}

	if err := json.Unmarshal(body, &meta); err != nil {

		return "", fmt.Errorf("decoding OIDC discovery document: %w", err)
	}

	if meta.TokenEndpoint == "" {

		return "", fmt.Errorf("OIDC discovery document carried no token_endpoint")
	}

	return meta.TokenEndpoint, nil
}

// AuthConfig is the union of the auth-related flags a generator exposes. A set ClientID selects the
// OIDC client-credentials grant; otherwise a non-empty Token selects the static source; otherwise no
// auth is configured (the service is unauthenticated).
type AuthConfig struct {
	Token string
	ClientCredentialsConfig
}

// NewSource returns a Source for the configured auth, or a nil Source when none is configured, so a
// caller can add the interceptor only when there is something to send.
func NewSource(ctx context.Context, ac AuthConfig) (Source, error) {
	if ac.ClientID != "" {

		return NewClientCredentialsSource(ctx, ac.ClientCredentialsConfig)
	}

	if ac.Token != "" {

		return NewStaticSource(ac.Token), nil
	}

	return nil, nil
}

// BearerInterceptor returns a unary client interceptor that resolves a token from src and stamps it
// onto every RPC's outgoing metadata. An empty token is left off (so a misconfigured static source
// simply sends no header rather than "Bearer ").
func BearerInterceptor(src Source) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		token, err := src.Token(ctx)
		if err != nil {

			return fmt.Errorf("obtaining bearer token: %w", err)
		}

		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
