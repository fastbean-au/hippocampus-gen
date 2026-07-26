package client

import (
	"context"
	"testing"

	"github.com/spf13/pflag"

	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
)

func TestRegisterAuthFlags(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterAuthFlags(fs)

	for _, name := range []string{
		"token", "oidc-issuer", "oidc-token-url", "oidc-client-id",
		"oidc-client-secret", "oidc-scope", "oidc-audience",
	} {
		if fs.Lookup(name) == nil {
			t.Errorf("expected flag %q to be registered", name)
		}
	}
}

func TestDialOptions(t *testing.T) {
	cases := []struct {
		name    string
		ac      oidc.AuthConfig
		wantLen int
		wantErr bool
	}{
		{
			name:    "no auth is plain transport only",
			ac:      oidc.AuthConfig{},
			wantLen: 1,
		},
		{
			name:    "static token adds the interceptor",
			ac:      oidc.AuthConfig{Token: "abc"},
			wantLen: 2,
		},
		{
			name: "client credentials adds the interceptor",
			ac: oidc.AuthConfig{ClientCredentialsConfig: oidc.ClientCredentialsConfig{
				TokenURL: "https://issuer.example/token", ClientID: "c", ClientSecret: "s",
			}},
			wantLen: 2,
		},
		{
			name: "misconfigured client credentials errors",
			ac: oidc.AuthConfig{ClientCredentialsConfig: oidc.ClientCredentialsConfig{
				ClientID: "c", ClientSecret: "s", // neither issuer nor token url
			}},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, err := DialOptions(context.Background(), c.ac)

			if c.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("DialOptions: %s", err)
			}

			if len(opts) != c.wantLen {
				t.Fatalf("expected %d dial options, got %d", c.wantLen, len(opts))
			}
		})
	}
}
