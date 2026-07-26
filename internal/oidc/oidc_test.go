package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// tokenServer is a fake OIDC provider: it serves a discovery document pointing at its own token
// endpoint, and mints a numbered token per client-credentials request so a refresh is observable. It
// records the last form it received so a test can assert the grant parameters.
type tokenServer struct {
	srv       *httptest.Server
	issued    int
	expiresIn int64
	lastForm  url.Values
}

func newTokenServer(t *testing.T, expiresIn int64) *tokenServer {
	t.Helper()

	ts := &tokenServer{expiresIn: expiresIn}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token_endpoint": ts.srv.URL + "/token"})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.lastForm = r.Form
		ts.issued++

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok-%d", ts.issued),
			"expires_in":   ts.expiresIn,
			"token_type":   "Bearer",
		})
	})

	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)

	return ts
}

func TestStaticSource(t *testing.T) {
	got, err := NewStaticSource("abc").Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %s", err)
	}

	if got != "abc" {
		t.Fatalf("expected abc, got %q", got)
	}
}

func TestClientCredentialsDiscoversAndSendsGrant(t *testing.T) {
	ts := newTokenServer(t, 3600)

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		Issuer:       ts.srv.URL,
		ClientID:     "gen",
		ClientSecret: "shh",
		Scope:        "openid",
		Audience:     "https://api.example",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %s", err)
	}

	if got != "tok-1" {
		t.Fatalf("expected tok-1, got %q", got)
	}

	// The grant carried every configured parameter, including the audience Auth0 needs.
	for k, want := range map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     "gen",
		"client_secret": "shh",
		"scope":         "openid",
		"audience":      "https://api.example",
	} {
		if got := ts.lastForm.Get(k); got != want {
			t.Errorf("form %q = %q, want %q", k, got, want)
		}
	}
}

func TestClientCredentialsCachesUntilExpiry(t *testing.T) {
	ts := newTokenServer(t, 3600)

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		TokenURL: ts.srv.URL + "/token", ClientID: "gen", ClientSecret: "shh",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := src.Token(context.Background()); err != nil {
			t.Fatalf("Token: %s", err)
		}
	}

	if ts.issued != 1 {
		t.Fatalf("expected the token to be fetched once and cached, got %d fetches", ts.issued)
	}
}

func TestClientCredentialsRefreshesWhenExpired(t *testing.T) {
	// A token that is already inside the refresh-skew window is not reused: the next Token fetches.
	ts := newTokenServer(t, 3600)

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		TokenURL: ts.srv.URL + "/token", ClientID: "gen", ClientSecret: "shh",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %s", err)
	}

	src.expiry = time.Now().Add(refreshSkew / 2)

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %s", err)
	}

	if got != "tok-2" || ts.issued != 2 {
		t.Fatalf("expected a refresh to tok-2 (2 fetches), got %q after %d fetches", got, ts.issued)
	}
}

func TestNewClientCredentialsSourceValidates(t *testing.T) {
	cases := []struct {
		name string
		cfg  ClientCredentialsConfig
	}{
		{"no client id", ClientCredentialsConfig{Issuer: "https://x", ClientSecret: "s"}},
		{"no secret", ClientCredentialsConfig{Issuer: "https://x", ClientID: "c"}},
		{"no endpoint", ClientCredentialsConfig{ClientID: "c", ClientSecret: "s"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewClientCredentialsSource(context.Background(), c.cfg); err == nil {
				t.Fatalf("expected an error for %s", c.name)
			}
		})
	}
}

func TestClientCredentialsTokenEndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		TokenURL: srv.URL, ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected an error from a 401 token endpoint")
	}
}

func TestClientCredentialsNoAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer"})
	}))
	defer srv.Close()

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		TokenURL: srv.URL, ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected an error when the response carries no access_token")
	}
}

func TestClientCredentialsBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		TokenURL: srv.URL, ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected an error decoding a non-JSON token response")
	}
}

func TestClientCredentialsDefaultTTL(t *testing.T) {
	// A response with no expires_in must still expire, so the cached token is refreshed eventually.
	ts := newTokenServer(t, 0)

	src, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
		TokenURL: ts.srv.URL + "/token", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("NewClientCredentialsSource: %s", err)
	}

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %s", err)
	}

	if !src.expiry.After(time.Now().Add(defaultTokenTTL - time.Minute)) {
		t.Fatalf("expected a default TTL roughly %s out, got expiry %s", defaultTokenTTL, src.expiry)
	}
}

func TestDiscoveryErrors(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"non-200":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusNotFound) },
		"bad json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) },
		"no token endpoint": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "x"})
		},
	}

	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h(w, r)
			}))
			defer srv.Close()

			_, err := NewClientCredentialsSource(context.Background(), ClientCredentialsConfig{
				Issuer: srv.URL, ClientID: "c", ClientSecret: "s",
			})
			if err == nil {
				t.Fatalf("expected a discovery error for %s", name)
			}
		})
	}
}

func TestNewSourceSelection(t *testing.T) {
	ts := newTokenServer(t, 3600)

	// A client id selects the client-credentials grant.
	src, err := NewSource(context.Background(), AuthConfig{
		ClientCredentialsConfig: ClientCredentialsConfig{Issuer: ts.srv.URL, ClientID: "c", ClientSecret: "s"},
	})
	if err != nil {
		t.Fatalf("NewSource (oidc): %s", err)
	}

	if _, ok := src.(*ClientCredentialsSource); !ok {
		t.Fatalf("expected a *ClientCredentialsSource, got %T", src)
	}

	// A bare token selects the static source.
	src, err = NewSource(context.Background(), AuthConfig{Token: "abc"})
	if err != nil {
		t.Fatalf("NewSource (static): %s", err)
	}

	if _, ok := src.(StaticSource); !ok {
		t.Fatalf("expected a StaticSource, got %T", src)
	}

	// Nothing configured yields a nil source, so a caller adds no interceptor.
	src, err = NewSource(context.Background(), AuthConfig{})
	if err != nil {
		t.Fatalf("NewSource (none): %s", err)
	}

	if src != nil {
		t.Fatalf("expected a nil source when nothing is configured, got %T", src)
	}
}

// captureInvoker records the outgoing metadata a client interceptor produced.
func captureInvoker(seen *metadata.MD) grpc.UnaryInvoker {
	return func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		*seen = md

		return nil
	}
}

func TestBearerInterceptorStampsToken(t *testing.T) {
	var seen metadata.MD

	err := BearerInterceptor(NewStaticSource("abc"))(context.Background(), "/svc/M", nil, nil, nil, captureInvoker(&seen))
	if err != nil {
		t.Fatalf("interceptor: %s", err)
	}

	if got := seen.Get("authorization"); len(got) != 1 || got[0] != "Bearer abc" {
		t.Fatalf("expected [Bearer abc], got %v", got)
	}
}

func TestBearerInterceptorOmitsEmptyToken(t *testing.T) {
	var seen metadata.MD

	err := BearerInterceptor(NewStaticSource(""))(context.Background(), "/svc/M", nil, nil, nil, captureInvoker(&seen))
	if err != nil {
		t.Fatalf("interceptor: %s", err)
	}

	if got := seen.Get("authorization"); len(got) != 0 {
		t.Fatalf("expected no authorization header for an empty token, got %v", got)
	}
}

// errSource always fails, so the interceptor's error path can be exercised.
type errSource struct{}

func (errSource) Token(context.Context) (string, error) {
	return "", fmt.Errorf("boom")
}

func TestBearerInterceptorPropagatesSourceError(t *testing.T) {
	invoked := false

	invoker := grpc.UnaryInvoker(func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		invoked = true

		return nil
	})

	err := BearerInterceptor(errSource{})(context.Background(), "/svc/M", nil, nil, nil, invoker)
	if err == nil {
		t.Fatal("expected the source error to propagate")
	}

	if invoked {
		t.Fatal("expected the RPC not to be invoked when the token could not be obtained")
	}
}
