package trace

// The realistic vocabulary: the topic terms a memory draws on when the trace is rendered as an
// engineer's working notes rather than as synthetic syllables. body.go holds the synthetic
// vocabulary, which stays the default for scoring runs; this one is for demonstrations, where a
// person has to be able to read a memory and search for it.
//
// Three constraints shape the list, and all three are load-bearing:
//
//  1. Every term is a SINGLE alphanumeric token. SQLite's FTS5 unicode61 tokeniser and
//     OpenSearch's standard analyser both split on hyphen and underscore, so a compound term like
//     "auth-retry" would silently become two very common tokens and Config.MemoriesPerTerm would
//     stop meaning anything. That is why the list holds "keyrotation" and "tokenbucket" rather
//     than the hyphenated forms a person would normally write.
//
//  2. Terms are grouped by working area, and an area doubles as a directory name in the path a
//     memory names. Drawing a memory's terms from its own area is what stops the generated notes
//     reading like "internal/auth/handler.go: spill is computed before expiry is applied" - the
//     terms and the file agree about what the memory is about.
//
//  3. Terms are globally unique across areas. A term carried by two areas is carried by twice the
//     memories, which quietly halves its discriminating power and makes the vocabulary's size a
//     poor predictor of retrieval difficulty.
//
// The size is set by what Trace.vocabulary() asks for: Memories * TermsPerMemory / MemoriesPerTerm,
// which at the agent defaults (20,000 memories, 4 terms each, 100 memories per term) is 800 terms.
// This list is exactly 800 - 20 areas of 40 - so the default configuration is honoured without the
// generator having to reuse a term. Ask for a more discriminating vocabulary than this list can
// serve and the terms simply spread thinner; that is a demonstration concern rather than a
// measurement one, since --live never scores.
//
// Spelling is British where the two differ, matching the prose in the repository's own docs.

// area is one working area of the imaginary system the generated agent works on. Name doubles as
// the directory segment in generated paths; Terms are the topic vocabulary for memories filed
// there.
type area struct {
	Name  string
	Terms []string
}

// workAreas is a slice rather than a map because Generate is deterministic: the same Config and
// Seed must always produce the same trace, and Go randomises map iteration order.
var workAreas = []area{
	{
		Name: "auth",
		Terms: []string{
			"token", "bearer", "refresh", "claim", "scope", "audience", "issuer", "nonce",
			"jwks", "keypair", "rotation", "revocation", "introspection", "oidc", "saml", "mfa",
			"totp", "passkey", "credential", "principal", "impersonation", "consent",
			"authorization", "authentication", "subject", "signature", "expiry", "clockskew",
			"pkce", "callback", "redirect", "grant", "assertion", "federation", "realm",
			"tenancy", "privilege", "escalation", "allowlist", "denylist",
		},
	},
	{
		Name: "cache",
		Terms: []string{
			"eviction", "ttl", "warmup", "invalidation", "stampede", "thrash", "hitrate",
			"prefetch", "writeback", "writethrough", "lru", "lfu", "bloom", "memoization",
			"staleness", "coherence", "admission", "promotion", "demotion", "pinning",
			"residency", "workingset", "hotkey", "coldstart", "keyspace", "footprint",
			"lifetime", "refreshahead", "dedupe", "digest", "fingerprint", "negativehit",
			"tiering", "spill", "eager", "lazy", "cascade", "purge", "sweep", "reheat",
		},
	},
	{
		Name: "scheduler",
		Terms: []string{
			"cron", "backlog", "starvation", "preemption", "priority", "fairness", "quantum",
			"tick", "drift", "jitter", "deadline", "slack", "affinity", "placement", "binpack",
			"spread", "cooldown", "debounce", "coalescing", "fanout", "concurrency",
			"parallelism", "semaphore", "worker", "pool", "lease", "heartbeat", "requeue",
			"deferral", "timer", "wheel", "tumbling", "elastic", "autoscale", "saturation",
			"headroom", "throughput", "latency", "queueing", "quiesce",
		},
	},
	{
		Name: "ingest",
		Terms: []string{
			"batch", "stream", "chunk", "offset", "watermark", "checkpoint", "backpressure",
			"buffering", "flush", "drain", "ordering", "idempotency", "envelope", "payload",
			"schema", "validation", "rejection", "deadletter", "retryqueue", "poison", "ack",
			"nack", "redelivery", "atleastonce", "atmostonce", "exactlyonce", "partition",
			"keying", "skew", "lag", "throttling", "sampling", "enrichment", "normalisation",
			"ingestion", "sink", "source", "connector", "pipeline", "staging",
		},
	},
	{
		Name: "storage",
		Terms: []string{
			"compaction", "tombstone", "vacuum", "fsync", "wal", "journal", "btree", "lsm",
			"sstable", "manifest", "blockcache", "page", "extent", "fragmentation", "defrag",
			"checksum", "corruption", "torn", "durability", "fdatasync", "mmap", "pagecache",
			"inode", "allocation", "freelist", "overwrite", "append", "truncate", "sparse",
			"reclaim", "compression", "encryption", "blob", "chunkstore", "coldstorage",
			"retention", "archival", "restore", "integrity", "blockdevice",
		},
	},
	{
		Name: "indexer",
		Terms: []string{
			"bm25", "tokeniser", "analyser", "stemming", "lemmatisation", "stopword", "ngram",
			"inverted", "posting", "segment", "merge", "reindex", "mapping", "shard", "replica",
			"routing", "scoring", "boost", "relevance", "recall", "precision", "facet",
			"aggregation", "highlight", "synonym", "fuzzy", "prefix", "wildcard", "phrase",
			"proximity", "vector", "embedding", "ann", "hnsw", "cosine", "reranking", "hybrid",
			"lexical", "semantic", "cardinality",
		},
	},
	{
		Name: "transport",
		Terms: []string{
			"grpc", "http2", "keepalive", "handshake", "tls", "mtls", "alpn", "cipher",
			"certificate", "chain", "ocsp", "sni", "multiplexing", "framing", "windowsize",
			"flowcontrol", "halfclose", "trailer", "metadata", "timeout", "interceptor", "unary",
			"streaming", "backoff", "reconnect", "resolver", "loadbalancer", "roundrobin",
			"pickfirst", "subchannel", "channelz", "proxy", "tunnel", "socket", "buffer",
			"nagle", "mtu", "retransmit", "congestion", "goaway",
		},
	},
	{
		Name: "consolidation",
		Terms: []string{
			"decay", "significance", "sleep", "cycle", "aggressiveness", "halflife",
			"forgetting", "reinforcement", "ripeness", "summarisation", "candidate",
			"condensation", "linkage", "association", "damping", "ageing", "unitofage",
			"capacity", "pressure", "sweeping", "pruning", "merging", "abstraction", "gist",
			"salience", "rehearsal", "replay", "encoding", "retrieval", "recency", "primacy",
			"interference", "recollection", "familiarity", "trace", "engram", "priming",
			"chunking", "forgetcurve", "spacing",
		},
	},
	{
		Name: "telemetry",
		Terms: []string{
			"span", "tracing", "tailsampling", "exemplar", "histogram", "bucket", "quantile",
			"percentile", "gauge", "counter", "labelset", "attribute", "exporter", "collector",
			"batcher", "propagation", "baggage", "context", "traceid", "spanid", "parentspan",
			"otlp", "prometheus", "scrape", "alerting", "threshold", "anomaly", "dashboard",
			"sli", "slo", "errorbudget", "apdex", "heatmap", "downsampling", "rollup",
			"instrumentation", "logline", "logfield", "severity", "redaction",
		},
	},
	{
		Name: "migration",
		Terms: []string{
			"backfill", "dualwrite", "shadowread", "cutover", "rollback", "rollforward",
			"versioning", "ddl", "alter", "column", "nullable", "default", "constraint",
			"foreignkey", "uniqueindex", "lock", "blocking", "online", "expand", "contract",
			"downtime", "reconcile", "divergence", "parity", "verification", "dryrun",
			"batchsize", "pacing", "resume", "idempotent", "checkpointing", "legacy",
			"deprecation", "sunset", "compatibility", "forwardcompat", "backwardcompat",
			"schemaversion", "seeding", "fixture",
		},
	},
	{
		Name: "ratelimit",
		Terms: []string{
			"tokenbucket", "leakybucket", "slidingwindow", "fixedwindow", "burst", "refill",
			"allowance", "throttle", "shedding", "degradation", "fairshare", "perkey",
			"perclient", "global", "distributed", "syncing", "clientid", "retryafter", "headers",
			"statuscode", "toomanyrequests", "exemption", "bypass", "penalty", "decorrelated",
			"adaptive", "aimd", "circuitbreaker", "halfopen", "tripped", "recovery",
			"healthcheck", "probe", "bulkhead", "isolation", "budget", "hedging",
			"concurrencylimit", "admissioncontrol", "prioritisation",
		},
	},
	{
		Name: "webhook",
		Terms: []string{
			"delivery", "attempt", "hmac", "secret", "keyrotation", "replayattack", "endpoint",
			"subscription", "event", "filtering", "transform", "batching", "sequencing",
			"broadcast", "quarantine", "schedule", "exponential", "maxattempts", "responsetime",
			"acknowledgement", "idempotencykey", "duplicate", "freshness", "validationerror",
			"callbackurl", "tls13", "ipallowlist", "egress", "firewall", "proxying",
			"latencyspike", "stall", "disabled", "autodisable", "failurerate", "alert",
			"notification", "consumerlag", "payloadschema", "oversized",
		},
	},
	{
		Name: "quota",
		Terms: []string{
			"limit", "usage", "metering", "overage", "entitlement", "plan", "tier", "seat",
			"reservation", "consumption", "accrual", "reset", "period", "rollover", "softlimit",
			"hardlimit", "breach", "warning", "enforcement", "waiver", "proration", "trueup",
			"forecast", "projection", "burndown", "headcount", "unit", "credit", "debit",
			"balance", "ledger", "reconciliation", "dispute", "adjustment", "audit", "statement",
			"report", "ceiling", "floor", "utilisation",
		},
	},
	{
		Name: "snapshot",
		Terms: []string{
			"incremental", "differential", "fullbackup", "restorepoint", "pointintime", "cow",
			"clone", "image", "volume", "mount", "unmount", "consistency", "quiescing", "freeze",
			"thaw", "crashconsistent", "appconsistent", "catalogue", "lifecyclerule", "policy",
			"generations", "lineage", "verify", "hashing", "deduplication", "ratio", "offsite",
			"crossregion", "envelopekey", "vaulting", "coldtier", "glacier", "recalltime", "rpo",
			"rto", "drill", "failback", "failover", "orphan", "garbage",
		},
	},
	{
		Name: "parser",
		Terms: []string{
			"lexer", "grammar", "ast", "cst", "terminal", "nonterminal", "production",
			"recursion", "leftrecursion", "precedence", "associativity", "ambiguity",
			"backtracking", "lookahead", "peek", "consume", "expect", "unexpected", "syntaxerror",
			"errorrecovery", "position", "rangecheck", "linecol", "escape", "quoting", "unicode",
			"bom", "charset", "newline", "whitespace", "comment", "directive", "macro",
			"expansion", "substitution", "interpolation", "literal", "identifier", "keyword",
			"operator",
		},
	},
	{
		Name: "dispatcher",
		Terms: []string{
			"dispatchtable", "handler", "middleware", "pipelinestage", "matcher", "pattern",
			"glob", "methodnotallowed", "notfound", "fallback", "defaultroute", "prefixtree",
			"radix", "trie", "conflict", "shadowing", "registration", "deregistration",
			"hotreload", "apiversion", "canary", "bluegreen", "weighted", "sticky", "affinitykey",
			"draining", "graceful", "shutdown", "startup", "readiness", "liveness", "rampup",
			"queuedepth", "executor", "handoff", "cancellation", "propagate", "panic", "recover",
			"errormapping",
		},
	},
	{
		Name: "registry",
		Terms: []string{
			"discovery", "entry", "leaseexpiry", "deregister", "stale", "health", "endpoints",
			"watch", "subscribe", "notify", "longpoll", "etag", "revision", "generation",
			"compareandswap", "cas", "optimistic", "pessimistic", "resolution", "softdelete",
			"namespace", "label", "selector", "annotation", "ownership", "hierarchy",
			"inheritance", "override", "defaulting", "admissionwebhook", "mutation", "immutable",
			"finaliser", "cascadedelete", "dangling", "gc", "resync", "informer", "reflector",
			"relist",
		},
	},
	{
		Name: "billing",
		Terms: []string{
			"invoice", "lineitem", "recurring", "midcycle", "coupon", "discount", "tax", "vat",
			"gst", "currency", "exchangerate", "rounding", "cents", "minorunits", "gateway",
			"charge", "capture", "preauth", "settlement", "chargeback", "arbitration", "refund",
			"partialrefund", "dunning", "dunningcycle", "declined", "insufficient", "expiredcard",
			"tokenisation", "pci", "vaulted", "receipt", "descriptor", "paymentevent",
			"settlementfile", "payout", "fee", "net", "gross", "mrr",
		},
	},
	{
		Name: "session",
		Terms: []string{
			"cookie", "samesite", "httponly", "secureflag", "domain", "cookiepath", "sessionid",
			"regeneration", "fixation", "hijacking", "csrf", "xsrf", "doublesubmit", "referrer",
			"origin", "cors", "preflight", "withcredentials", "sessionstore", "pinnedbackend",
			"idletimeout", "absolutetimeout", "sliding", "renewal", "logout", "singlesignout",
			"binding", "fingerprinting", "useragent", "ipchange", "concurrentlogin",
			"sessioneviction", "serialisation", "deserialisation", "atrest", "sizelimit",
			"chunked", "overflow", "sessionmigration", "revoked",
		},
	},
	{
		Name: "replication",
		Terms: []string{
			"primary", "standby", "follower", "leader", "election", "raft", "paxos", "epoch",
			"log", "logoffset", "commit", "apply", "quorum", "majority", "split", "splitbrain",
			"fencing", "stonith", "replicalag", "catchup", "shipping", "walshipping", "semisync",
			"asynchronous", "synchronous", "linearizable", "eventual", "monotonic",
			"readyourwrites", "staleread", "promotiontime", "demote", "rejoin", "reseed",
			"divergentlog", "logtruncation", "heartbeatloss", "witness", "arbiter", "topology",
		},
	},
}
