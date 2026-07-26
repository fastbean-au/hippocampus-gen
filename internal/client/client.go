// Package client centralises the gRPC dial wiring shared by the generators: the common auth flag
// set and the dial options (transport credentials plus, when auth is configured, the bearer-token
// interceptor). Per-program viper reads stay in each command's main; this package takes explicit
// values so it never touches viper itself.
package client

import (
	"context"

	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
)

// RegisterAuthFlags declares the auth flags every generator shares, so a service that requires a
// token can be driven by any of them. They are inert by default (no auth), preserving the plain,
// unauthenticated behaviour when none is set.
func RegisterAuthFlags(fs *pflag.FlagSet) {
	fs.String("token", "", "static bearer token sent on every RPC when the service requires auth")
	fs.String("oidc-issuer", "", "OIDC issuer URL for a client-credentials grant; the token endpoint is discovered from it")
	fs.String("oidc-token-url", "", "OIDC token endpoint URL, overriding discovery from --oidc-issuer")
	fs.String("oidc-client-id", "", "OIDC client id for a client-credentials grant")
	fs.String("oidc-client-secret", "", "OIDC client secret for a client-credentials grant")
	fs.String("oidc-scope", "", "OIDC scopes to request in a client-credentials grant")
	fs.String("oidc-audience", "", "OIDC audience (Auth0 needs it to mint a JWT access token; Keycloak ignores it)")
}

// DialOptions builds the gRPC dial options for the target service. It always uses plaintext
// transport credentials (the generators talk to a demonstration instance), and adds the
// bearer-token interceptor when ac configures a token source. A nil source (no auth configured)
// leaves the options plain.
func DialOptions(ctx context.Context, ac oidc.AuthConfig) ([]grpc.DialOption, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	src, err := oidc.NewSource(ctx, ac)
	if err != nil {

		return nil, err
	}

	if src != nil {
		opts = append(opts, grpc.WithUnaryInterceptor(oidc.BearerInterceptor(src)))
	}

	return opts, nil
}
