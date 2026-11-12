# Shared MCP and checkpointed capsules

For per-backend service plans, service-account JWT authorization, federated
client-credentials exchange, and selected-identity capsules, see
[MCP federation](mcp-federation.md). The global-list behavior below applies to
legacy entries without `plan_id`.

Operators configure `mcp_servers` once for the deployment. Every service instance
can access every configured server; per-instance server selection is not part of
the MVP. Each entry has `name`, HTTP(S) `url`, and optional static `headers`.
Names must be unique. Upstream headers are literal values, not CEL expressions.
The gateway strips the consumer's authorization header before upstream calls;
configure upstream credentials explicitly in the entry's headers.

Bindings and service keys include `mcp_url` when the list is nonempty, alongside
the existing inference `url`, shared `api_key`, and `trusted_ca`. Clients send
the same Bearer key to `/mcp`. Unbinding preserves the key; deprovisioning revokes
it. MCP access does not consume the inference USD allowance.

## Runtime

Each gateway web container runs a Go `capsule-extmcp` CF sidecar. HTTP extAuthz
on `127.0.0.1:50052` checks every MCP HTTP request against the live broker-managed
`llm.apiKey` resource in PostgreSQL, including initialize, streams, and session
deletion. The ExtMcp processor uses loopback gRPC on port 50051. Both checks fail
closed. No external route is created for the sidecar.

The request hook writes a signed planned capsule and returns its ID as dynamic
metadata. The pinned gateway exposes that metadata as
`mcpGuardrails.capsule_plan`, which its response hook sends back to the sidecar.
Responses are correlated by this ID, never by order or a process-local queue.
The plan supplies the service-instance identity and backend for the outcome.

Capsules commit to the installation, service-instance UUID, org/space IDs, tool,
backend, phase, and request/result digests. Business request/result bodies are
not retained. A normal result produces a confirmed capsule; `isError: true`
produces a failed capsule. The pinned gateway skips response hooks for JSON-RPC
errors and transport failures: these leave a planned capsule only. A response
recording error does not undo a tool action that already executed.

## Persistence and checkpoints

Request/result bodies are decoded with `Decoder.UseNumber`, converted with
capsule-emit's `CanonicalizeForDigest`, and committed with `DigestJSON`.
The same canonicalized value supplies input/output and effect-response digests.
Signed metadata identifies this format as
`gateway.body_encoding: "mcp-json/canonicalize-for-digest/v1"`.

Decimal and exponent tokens become RFC 8785 decimal strings: `0.5` and `0.50`
become `"0.5"`, and `-0.0` becomes `"0"`. Numeric `0.5` and string `"0.5"`
therefore have the same digest. Integer tokens stay integers; `1` and `1.0`
digest differently. Whitespace and object-member order do not affect digests.
Integer tokens outside ±(2^53−1) and numbers overflowing binary64 are rejected.
Original bodies and their canonicalized values are not retained.

Earlier pre-production capsules used `mcp-json-bytes/base64/v1`, which committed
exact JSON bytes. Their signed `body_encoding` identifies that older format;
existing capsules are not rewritten.

The sidecar shares the gateway database and initializes its own additive tables:

- `capsule_store_capsules` / `capsule_store_artifacts`: upstream capsule-emit
  v0.5.0 PostgreSQL artifact storage, with exact signed bytes, envelope, inventory
  checksum, and original-retention support. The sidecar supplies no originals.
- `agw_capsules`: application metadata only, checksum, indexed
  installation/instance/time, and parent reference.
- `agw_capsule_entries`: dense ordered 32-byte capsule IDs and append times.
- `agw_capsule_logs`: durable log cursor and checkpoint metadata.
- `agw_capsule_nodes`: append-only MMR nodes.
- `agw_capsule_witnesses`: durable delivery attempts and verified receipts.

Upstream `PutTx`, application metadata, and CLL append commit in one READ COMMITTED
`database/sql` transaction. The SQL handle uses pgx's stdlib adapter over the
sidecar's existing connection pool; native pgx remains in use for authorization,
checkpoint state, and leadership. Duplicate byte-identical writes preserve their
original CLL position. No gateway tables are
modified by the sidecar. Database roles need CREATE TABLE/INDEX privileges for
initial provisioning. Back up these tables together with gateway configuration.

The Go Action State libraries create/verify capsules, build the MMR, sign
checkpoints, and submit/verify witness receipts. The PostgreSQL backend implements
the public CLL contract and uses the public memory backend to validate state
transitions. CLL storage remains application-owned. Signed artifact persistence
uses the upstream `artifact/postgres` implementation of `artifact.Store` and
`artifact.TxStore`, delivered in
[capsule-emit #454](https://github.com/action-state-group/capsule-emit/issues/454).
The artifact namespace is the lowercase hexadecimal SHA-256 of `CAPSULE_LOG_ID`,
explicitly giving each producer log its own artifact collection. The application
repository validates indexed attribution against the signed Capsule on save/read;
it does not expose general padding or purge operations. The test suite runs both
the CLL and artifact shared conformance suites against disposable PostgreSQL.

### Pre-production storage cutover

This is a breaking change from the previous application-owned artifact format;
there is no migration or backward-compatible reader. Stop old sidecars and start
with an empty capsule evidence store, resetting the capsule application and CLL
tables together. Preserve gateway configuration and its managed lifecycle
resources. Old evidence can be archived separately, but is not imported by the
sidecar. Do not mix old and new sidecar versions against the same evidence store.

One sidecar owns a PostgreSQL session advisory lock per log and runs background
checkpoint and witness workers. Other sidecars keep appending normally. Workers
are canceled on leader-connection loss; a replacement resumes durable state.
Witness delivery is at-least-once, so the chosen witness must accept duplicate
submissions safely. Witness outages delay receipts, not tool calls.

The checkpoint runner polls every five seconds, using upstream's default cadence
(100 records or 900 seconds). `CAPSULE_CHECKPOINT_ENTRIES` overrides the count for
testing. MMR state is reconstructed from durable nodes; current upstream's runner
loads the full node set, so memory and checkpoint latency grow with ledger size.
Measure this before selecting production retention/rotation and capacity limits.

## Operator settings

- `mcp_session_key`: stable private 32-byte hexadecimal AES-256-GCM key, separate
  from the capsule signing seed and shared across gateway replicas. The launcher
  refuses to enable MCP without a valid `SESSION_KEY`. This authenticates resumed
  session state; changing the key invalidates existing sessions.

- `capsule_signing_seed`: required 64-hex-character Ed25519 seed, stored privately.
  Keep the same seed across replicas and restarts. Rotation requires coordinated
  trusted-key/checkpoint handling; replacing it without that fails verification.
- `installation`: also used as `CAPSULE_LOG_ID`; keep it stable across redeploys.
- `capsule_witness_url`: optional external witness base URL (`POST /checkpoints`).
- `capsule_witness_public_key`: pinned 32-byte Ed25519 public key, hexadecimal;
  required when a witness URL is configured.

With no witness URL, the log still creates signed checkpoints, but is not
externally witnessed. A recipient must obtain trusted producer/witness keys
through an established channel; a self-presented key alone does not establish
operator identity. A frontend/query API is not shipped in this MVP; the database
index prepares service-instance filtering.

## Development and verification

Use Go 1.27.0 for the sidecar (`GOTOOLCHAIN=go1.27.0`); dependencies are pinned.
Run `go generate ./plugin` in `src/capsule-extmcp` to regenerate the version-pinned
Counterfeiter fakes. Ginkgo/Gomega specs cover transport behavior, real PostgreSQL
atomicity/concurrency, upstream CLL conformance (including reopen), leader-session
loss, and HTTP witness retries with signed receipt verification. The gateway
integration spec runs the pinned binary in a cflinuxfs5 Docker container.

`devbox run test` runs Go unit/contract suites; `devbox run acceptance` checks the
packaged binaries with real PostgreSQL and deterministic inference/MCP fixtures.
Tests require PostgreSQL CLI tools and Docker. No real providers or external
witnesses are contacted.
