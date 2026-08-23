# hippocampus-gen

Test data generator for Hippocampus. These programs will create sample or test data for use with the [Hippocampus](https://github.com/fastbean-au/hippocampus) service.

The module builds against the published `github.com/fastbean-au/hippocampus` contract (the version pinned in `go.mod`). Because that module is private, set `GOPRIVATE=github.com/fastbean-au/*` so `go` fetches it directly rather than through the public proxy and checksum database. Each generator takes `-s <host:port>` for the target gRPC address (default `localhost:50051`). By default they speak plain, unauthenticated gRPC; see [Authentication](#authentication) to drive a service that requires a bearer token. See the service's [Demonstrations](https://github.com/fastbean-au/hippocampus/blob/main/docs/demonstrations.md) guide for worked end-to-end examples in embedded and centralised modes.

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

## Group scoping

If the target service issues [group-scoped tokens](https://github.com/fastbean-au/hippocampus/blob/main/docs/configuration.md#group-scoping), a token may write only the group labels it holds. Every generator takes `--group` for this:

```bash
# a token scoped to "demo": file everything under that label
go run ./cmd/logs -s localhost:50051 -n 3000 -d 20 --group demo --token "$SCOPED_TOKEN"
```

**`logs` needs it; `book` and `random` usually do not.** The logs generator stamps each line's _service_ as its group by default, so against a scoped token every write is refused with `PermissionDenied` — `--group` is what resolves that, and the service is recorded as `metadata` either way, so it stays filterable (`?metadata=service%3Dauth`). The other two leave the group unset, which a scoped token handles by itself: the service stamps the token's own group. Set `--group` there only when the token carries **several** groups (the service cannot choose between them) or when the data should be filed under a specific label.

Two calls act on the whole store and are refused to a scoped token whatever its tier: `Sleep` and `Purge`. So:

- `book --summarize` nudges a consolidation cycle before asking for candidates. Under a scoped token that nudge is refused; the pass **warns and carries on** with the candidate list as it stands, since the cycle is an optimisation rather than a precondition.
- `book --reset` purges the store, and needs an **unscoped** admin token.

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

### Live, paced, and looping (the 24-hour showcase)

By default the book is laid across a back-dated timeline and streamed in a burst. For a hosted showcase that _creates over time → summarises → decays_, and repeats each day, these flags reshape the run:

- `--live` — stamp each write at the current time instead of back-dating, so the memories age in real wall-clock (and, with a compressed decay clock on the service, ripen for consolidation and summarisation within the run).
- `--pace-window <dur>` — spread the load across this window rather than bursting it, so events and memories appear _over time_ (e.g. `--pace-window 2h`).
- `--loop` with `--period <dur>` (default `24h`) — run continuously, one cycle per period, until interrupted (Ctrl-C / SIGTERM stops it promptly, even mid-load).
- `--reset` — purge the store at the start of each cycle, for a clean, deterministic reload each period. Purge is admin-tier, so this needs an **admin** token (see [Authentication](#authentication)).

```bash
# A self-perpetuating book showcase: each day, purge, reload the novel spread across two hours
# with live timestamps, then summarise the ripe events.
go run cmd/book/main.go -s localhost:50051 \
  --loop --period 24h --reset --pace-window 2h --live --summarize \
  --token "$(hippocampus --mint-token --role admin -c config.json)"
```

## Logs

A log-shaped generator: each synthetic log line becomes a memory whose significance is derived from the line's **level** (`DEBUG` lowest … `FATAL` highest), tagged with its emitting **service** and level as `metadata` (and, by default, with the service as the `group` label too), with lines grouped into one **event per service per day**. It is the demonstration that makes significance-driven forgetting concrete — after a sleep cycle, routine `DEBUG`/`INFO` noise is consolidated away first while `ERROR`/`FATAL` lines survive.

### Usage example

```bash
# 3,000 lines across 5 services over 20 days of history
go run cmd/logs/main.go -n 3000 -d 20
```

### Live trickle (the continuous-logs showcase)

By default the generator loads a fixed, back-dated batch and exits. `--live` instead trickles new lines at the current time, indefinitely — the counterpart to the book's daily reset, and the shape a hosted logs showcase wants: a steady stream where the service's own sleep cycle and capacity eviction reap the low-significance noise as the store fills, so it never grows without bound.

- `--live` — emit continuously at the current time instead of a one-shot back-dated batch.
- `--rate <n>` — approximate lines per minute (default `60`).
- `--duration <dur>` — stop after this long (default `0` = run until Ctrl-C / SIGTERM; either stops it promptly).

```bash
# A steady ~120 lines/min stream; leave it running and watch the sleep cycle forget the noise.
go run cmd/logs/main.go -s localhost:50051 --live --rate 120 \
  --token "$(hippocampus --mint-token --role writer -c config.json)"
```

The one-shot `-n`/`-d` flags are ignored under `--live`. There is no `--reset` here (unlike the book) — the point is a long-lived, ever-forgetting store, not a clean reload.

## Agent — the retention benchmark

The other three generators load data and let you watch consolidation act on it. This one asks a
different question, and it is the only one that produces a **number**: _of everything the store threw
away, how much did you actually need later?_

Stated that way, a forgetting store is a **cache replacement policy**, and cache replacement has a
settled way of being evaluated — replay a trace, then measure the hit rate against store size,
against baselines. That is what this is.

### `agentfit` — fitting the trace to a real agent

A benchmark whose author writes both the trace _and_ the ground truth proves nothing. So the
synthetic trace is built to reproduce a reuse distribution **measured from a real agent's session
transcripts** rather than one chosen to flatter the result.

```sh
go run ./cmd/agentfit \
  --transcripts ~/.claude/projects/-Users-me-src-myproject \
  --entity-prefix /myproject \
  --describe "myproject, Claude Code sessions" \
  --out internal/params/params.json
```

It reads Claude Code `.jsonl` transcripts, treats each path a tool call names as an _entity_, and
fits: the Zipf exponent over entity popularity, the once-only share, the session arrival process,
within-session co-occurrence (which becomes the link graph), and — the part that matters — the
**re-reference gap distribution**.

The committed [`internal/params/params.json`](internal/params/params.json) was fitted from 3,274
references across 77 sessions and 28 days, and is **compiled into the `agent` binary**, so the
container image needs no data files and `--params` is only for overriding it. **The corpus is private working data and does not ship; the fitted parameters
do**, which is what makes the benchmark auditable — the file records what it was fitted from and
when, and carries no paths or content, only aggregate statistics.

### `agent` — replaying and scoring

`cmd/agent` generates the workload from those parameters, replays it into a live instance in
compressed simulated time, and scores what survived against the standard cache-replacement baselines
at the same store size.

```sh
# Describe the workload without touching a service
go run ./cmd/agent --dry-run --memories 20000 --days 60

# Replay it into an instance and score the result
go run ./cmd/agent -s localhost:50051 --memories 20000 --days 60 \
  --sim-days-per-wall-minute 40 --out results.json
```

The instance needs `consolidation.unitsOfAgeInDays` matched to the replay speed; the harness reads
the setting back before starting and refuses a run that would measure a decay rate nobody chose,
naming the value to use. An optional `--control_address` points at a second, unbounded instance,
which serves as both the keep-everything ceiling and the search oracle the simulated baselines are
ranked from.

Results are reported for **two question kinds separately** — _next-touch_ ("what will be looked up
next", drawn from the reuse process) and _must-keep_ ("what matters regardless of access", drawn by
importance) — because averaging them hides which one a policy is bad at. The service-side write-up,
with the headline numbers and the limitations, is
[Retention quality](https://github.com/fastbean-au/hippocampus/blob/main/docs/retention.md).

### Why reuse is fitted as two modes

The single most important measurement is that re-reference is **bimodal**:

|                 | share of reuses | median gap |
| --------------- | --------------- | ---------- |
| same session    | 73%             | 11 seconds |
| a later session | 27%             | 17 hours   |

An agent re-reads something seconds later far more often than it returns to it days later — but the
second mode is the one a bounded store is judged on. **11.5% of reuses landed more than 24 hours
later and 3.2% more than a week later**, and a third of all entities were never referenced again at
all. A recency window sized for the burst discards precisely what is wanted next week; that gap is
the thing the benchmark measures.

The two modes are therefore fitted **separately**, split by whether the re-reference fell in the same
session — an observation rather than a chosen gap threshold — and never smoothed into one
distribution.

Each is stored two ways: as a log-normal summary, which is what a person reads, and as an empirical
inverse-CDF ladder, which is what the generator samples. Both are kept because the parametric fit
does not survive contact with the corpus — the cross-session mode's fitted parameters imply a 5.7
hour median against an observed 17, since that mode is itself a mixture of overlapping sessions and
genuine multi-day returns. The ladder is authoritative; the log-normal is commentary, and comparing
the two is how a badly-fitting mode announces itself.
