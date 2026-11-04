# Shared MCP and checkpointed capsules

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

Request/result digest preimages use the versioned wrapper
`{"encoding":"mcp-json-bytes/base64/v1","data":"<standard padded base64>"}`.
The data is the exact JSON bytes observed on the ExtMcp wire. This losslessly
supports decimal and large integer values while keeping the digest preimage
within AAC's string-only numeric-safe profile. The same wrapper is used for
input/output and effect-response digests; `gateway.body_encoding` identifies it
in the signed capsule. Whitespace or numeric spelling changes therefore change
the commitment. Original bodies are not retained, including their base64 form.

The sidecar shares the gateway database and initializes its own additive tables:

- `agw_capsules`: exact signed capsule/envelope bytes within a JSON storage
  record, checksum, indexed installation/instance/time, and parent reference.
- `agw_capsule_entries`: dense ordered 32-byte capsule IDs and append times.
- `agw_capsule_logs`: durable log cursor and checkpoint metadata.
- `agw_capsule_nodes`: append-only MMR nodes.
- `agw_capsule_witnesses`: durable delivery attempts and verified receipts.

Capsule persistence and CLL append commit in one transaction. Duplicate byte-
identical writes preserve their original CLL position. No gateway tables are
modified by the sidecar. Database roles need CREATE TABLE/INDEX privileges for
initial provisioning. Back up these tables together with gateway configuration.

The Go Action State libraries create/verify capsules, build the MMR, sign
checkpoints, and submit/verify witness receipts. The PostgreSQL backend implements
the public CLL contract and uses the public memory backend to validate state
transitions. Its format is application-owned; this is not the upstream MySQL DDL
and cannot be opened directly with that driver. The pinned capsule library
v0.3.0 provides artifact verification but does not publish the newer `artifact.Store`
interface documented on upstream main; capsule persistence uses our repository
interface and the pinned verification primitives.

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
