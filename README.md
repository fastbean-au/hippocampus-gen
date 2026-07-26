# hippocampus-gen

Test data generator for Hippocampus. These programs will create sample or test data for use with the [Hippocampus](https://github.com/fastbean-au/hippocampus) service.

The module builds against the published `github.com/fastbean-au/hippocampus` contract (currently `v0.14.1`). Because that module is private, set `GOPRIVATE=github.com/fastbean-au/*` so `go` fetches it directly rather than through the public proxy and checksum database. Each generator takes `-s <host:port>` for the target gRPC address (default `localhost:50051`). By default they speak plain, unauthenticated gRPC; see [Authentication](#authentication) to drive a service that requires a bearer token. See the service's [Demonstrations](https://github.com/fastbean-au/hippocampus/blob/main/docs/demonstrations.md) guide for worked end-to-end examples in embedded and centralised modes.

## Authentication

When the target service enables auth (`auth.method` `hmac` or `idp`), every generator accepts the same auth flags:

- `--token <jwt>` — a static bearer token (e.g. one minted by `hippocampus --mint-token`), sent on every RPC. Handy against an `hmac` instance.
- `--oidc-issuer` / `--oidc-client-id` / `--oidc-client-secret` — an OIDC **client-credentials** (machine-to-machine) grant. The token endpoint is discovered from the issuer (`<issuer>/.well-known/openid-configuration`), or set it directly with `--oidc-token-url`. The token is fetched on first use and refreshed automatically before it expires.
- `--oidc-scope` — scopes to request in the grant.
- `--oidc-audience` — the API audience. **Auth0** needs this set to the API identifier to mint a verifiable JWT access token; **Keycloak** ignores it.

The token's role must cover what the generator does: the `book` summarisation pass calls `Sleep` (admin), so it needs an **admin**-tier client; the plain loaders only store events and memories (**writer**). With no auth flags, behaviour is unchanged — plain, unauthenticated gRPC.

```bash
# hmac instance, static token:
go run ./cmd/logs -s localhost:50051 -n 3000 -d 20 --token "$(hippocampus --mint-token --role writer -c config.json)"

# idp instance (Keycloak / Auth0), machine-to-machine:
go run ./cmd/book -s localhost:50051 --summarize \
  --oidc-issuer https://issuer.example/realms/hippocampus \
  --oidc-client-id hippocampus-gen --oidc-client-secret "$SECRET" \
  --oidc-audience https://api.hippocampus.demo
```

## Random

The data produced by this utility is not meant to be particularly meaningful.

With this data generator, a wordlist is used to generate the event names, descriptions and bodies of memories. The wordlist used is the [MIT 10000 word list](https://www.mit.edu/~ecprice/wordlist.10000). The data itself is not meant to be particularly meaningful. However, this generator can be used to load test the Hippocampus service.

### Usage example

```bash
% go run cmd/random/main.go -e 135 -m 12000 -l 284 -p 13 -w 7
Starting worker: events (memories): 20 (223), memories 1492
Starting worker: events (memories): 19 (223), memories 1491
Starting worker: events (memories): 19 (223), memories 1491
Starting worker: events (memories): 19 (223), memories 1491
Starting worker: events (memories): 20 (223), memories 1492
Starting worker: events (memories): 19 (223), memories 1492
Starting worker: events (memories): 19 (222), memories 1491
```

## Book

The data produced by this utility is somewhat more useful in that it is not entirely meaningless data. The data used comes from the Charles Dickens novel Great Expectations.

This data generator uses the chapters as events, and each paragraph as a memory. The dates will increase chapter by chapter, paragraph by paragraph - this, obviously, will not follow the books' timelines accurately, and, the significance of events and memories will continue to be random.

### Usage example

```bash
go run cmd/book/main.go
```

The timeline is laid across a fixed window ending shortly before now (the service rejects memory timestamps more than a few minutes in the future), so the dates are internally consistent and in the past, though they still do not track the novel's own chronology.

### Summaries

With `--summarize` (`-S`), the generator exercises Hippocampus's summarization flow once the book is loaded: it triggers a consolidation cycle (`Sleep`), asks the service which events it considers ready to condense (`GetSummarizationCandidates`), and replaces each such event's memories with a single summary memory (`ReplaceMemoriesWithSummary`). The summary text is drawn from W.S. Gilbert's 1871 stage adaptation of the same novel, mapped scene by scene onto the chapters it retells. Candidates are only returned when the service is configured with `consolidation.summarizationMinMemories`, so point this at an instance that has it set — otherwise the pass finds nothing to summarize. Without the flag, behaviour is unchanged.

```bash
go run cmd/book/main.go --summarize
```

## Logs

A log-shaped generator: each synthetic log line becomes a memory whose significance is derived from the line's **level** (`DEBUG` lowest … `FATAL` highest), tagged with its emitting **service** via the `group` label, with lines grouped into one **event per service per day**. It is the demonstration that makes significance-driven forgetting concrete — after a sleep cycle, routine `DEBUG`/`INFO` noise is consolidated away first while `ERROR`/`FATAL` lines survive.

### Usage example

```bash
# 3,000 lines across 5 services over 20 days of history
go run cmd/logs/main.go -n 3000 -d 20
```