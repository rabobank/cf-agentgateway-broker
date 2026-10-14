# Agentgateway source investigation

## Scope and revisions

Inspected on 2026-10-13 for the MVP with a Go broker, shared instance API keys,
horizontally scalable gateways, BOSH DNS distribution, and singleton PostgreSQL
colocated with the broker. No UAA integration or broker database is proposed.

Source checkout: `/home/ruben/workspace/agentgateway`, cloned from
`https://github.com/agentgateway/agentgateway` because the directory did not exist.

- Inspected HEAD: `c0354ea426b25b4b3df357ba4b6afabcf4309660`.
- HEAD description: `v1.6.0-116-gc0354ea4`.
- Compared release: v1.6.0, `ea5608642b9d5c94c5baf5fd9f2f9849b808e963`.
- Checkout remains unmodified. This is source evidence, not a runtime spike.
- Rust/Cargo and PostgreSQL CLI were not on PATH; no build or integration tests ran.

Paths and line references below refer to inspected HEAD unless explicitly stated.

## 1. Replica configuration propagation exists

`crates/agentgateway/src/config_store.rs:180-210` opens a PostgreSQL `PgListener`,
subscribes to the configuration change channel, and forwards notifications from
other store instances into a local watch channel. Listener reconnection also
triggers a reload. Store writes issue `pg_notify` inside the same transaction as
resource changes (`:1601-1640`, `:1825-1835`). Local writes also signal changes.

`crates/agentgateway/src/state_manager.rs:150-245` consumes these events and
reloads the materialized file/database configuration. This applies to the file
watch path and the resource-watch path. These PostgreSQL mechanisms also exist in
v1.6.0.

**Implication:** Shared PostgreSQL has an actual cross-replica propagation mechanism;
the broker should not need to write each gateway independently. Activation and
revocation remain asynchronous. A successful resource API response is not an
acknowledgement that every gateway has applied the change.

**Runtime checks:** create/delete on replica A, observe authentication on B;
disconnect/reconnect B's database listener; scale out with existing resources;
check reload failures and propagation latency. All replicas must use compatible
file-owned baselines.

## 2. Budgets aggregate deltas and refresh shared totals

`crates/agentgateway/src/http/budget/database.rs`:

- `:15`, `:77-90`: periodic flush interval is five seconds.
- `:24-75`: initialize tables, prune expired rows, preload persisted usage.
- `:153-257`: snapshot pending deltas, commit increments, subtract successful
  snapshots, read persisted totals, reconcile local counters.
- `:281-312`: local amount becomes the persisted total plus local unflushed usage.

`crates/agentgateway/src/http/budget/postgres_upsert.sql` atomically adds usage for
the same budget window/unit. An older window cannot replace a newer one.
The flush reads shared totals even if this replica has no pending usage, provided
it has configured counters. Budget persistence code is unchanged from v1.6.0.

`crates/agentgateway/src/http/budget/mod.rs:629-768` contains a test using two
independent budget policies sharing SQLite. It checks delta addition, no repeated
charge on another flush, and shared-total refresh. It is useful source evidence,
but not a multi-process PostgreSQL test.

Admission (`budget/mod.rs:422-474`) checks memory; there is no transactional
reservation. Replica-local knowledge of other replicas' charges is delayed by
their flush/read timing. Overshoot depends on traffic, streams, timing, and replica
count; five seconds is a cadence, not a strict global convergence bound.

Database flush failures log warnings and preserve pending usage. Admission does
not consult database availability on every request. Ambiguous transaction outcomes
and retry behavior need fault testing; this code does not establish exactly-once
charging.

`crates/agentgateway-app/src/commands/run.rs:101` flushes budgets after proxy
termination. Verify stream draining and BPM termination timeouts in deployment.

**Implication:** Native eventual shared accounting is plausible for the accepted
MVP allowance semantics. It is not inherently a separate full allowance per
replica, but this still needs two-process PostgreSQL verification.

## 3. Budget identity is the key hash, not the resource UUID

`crates/agentgateway/src/http/apikey.rs:553-556` sets `api_key_id` to the key hash.
`crates/agentgateway/src/http/budget/mod.rs:295-303` builds counter identity from
that value and the budget name. Keys with budgets require `metadata.name`.

Keep the credential/hash, budget name, unit, and window stable during reconciliation.
Changing the key changes accounting identity; compatible configuration reloads
retain counters. Deleting/recreating the same hash can reattach old window usage.
Define service-instance recreation semantics before selecting deterministic keys.

## 4. API-backed metadata and credential recovery are feasible

The resource API stores JSON values in `agw_config_resources`. Collection listing
returns stored values, revisions, and timestamps (`ui.rs:265-274`, `:533-547`),
without converting keys through the runtime redacting serializer.

API-key resource values accept either `key` or `keyHash`
(`http/apikey.rs:454-485`). Therefore:

- Raw `key` submitted to the store remains retrievable through the management API.
- `keyHash` stores only a SHA-256 hash; the original secret cannot be recovered.
- The UI generates keys client-side (`ui/src/pages/Keys.tsx:1207-1219`); do not
  assume a gateway server-side credential-generation endpoint exists.
- Metadata is arbitrary JSON, with the `agentgateway.dev/` prefix reserved
  (`config_store.rs:1443-1492`). Use a broker-specific namespace for instance UUID,
  service/plan IDs, installation ownership, and a provision request fingerprint.

Possible credential strategies are raw-key recovery, deterministic derivation
with a stable CredHub secret, or an encrypted recoverable credential in resource
metadata. Select one explicitly after testing round trips. Keep management access
private and protect it appropriately for cross-VM broker access.

## 5. Metadata lookup and serialized CF lifecycle requests

`config_store.rs:1378-1399` assigns a fresh UUID for every API-key create, overriding
managed ID metadata. `ui.rs:653-670` requires an existing resource for API-key item
updates. Supplying a CF instance UUID in the request is therefore insufficient to
create an API key under that resource ID.

The exposed routes (`ui.rs:95-103`) provide collection GET/PUT and item PUT/DELETE;
there is no item GET route. A broker can list and filter metadata, but that is not
atomic uniqueness enforcement.

`config_store.rs:1601-1640` uses unconditional SQL upsert and increments revision.
The API does not use an expected revision or conditional-create precondition.
Validation and ownership checks happen before persistence (`ui.rs:570-617`).

**Consequences:**

- Concurrent read-then-create retries can produce duplicate resources for one
  CF instance, even when the credential is deterministically derived.
- Response loss can be recovered by metadata lookup, but that does not resolve
  simultaneous creates or conflicting request payloads.
- Updating a metadata map of bindings can lose concurrent changes.
- Stored revisions are informational, not compare-and-swap guarantees.
- Durable binding/operation state has no dedicated native resource type.

**MVP decision:** Rely on the target CAPI's serialization of lifecycle requests
for the same service instance. Do not require a gateway API extension for the
initial CF-only broker. Verify this assumption for provision/deprovision and the
chosen binding flow against the target CAPI; serialization of service-instance
operations should not be assumed to serialize all distinct binding operations.

Use API-key metadata as labels: installation identity, CF instance UUID, service
and plan IDs, and a provision fingerprint. List `llm.apiKey` resources and filter
client-side. There is no native label-selector/filter support in these handlers.
The gateway's random UUID is its resource identity; the CF UUID is a metadata
lookup identity. A unique match can recover a committed create after a lost
response. Compare its fingerprint before returning idempotent success; fail on
multiple matches rather than choosing one arbitrarily.

Bindings return the shared credential and unbind leaves the key unchanged. Avoid
a mutable binding map on the instance resource for the initial flow. Advertise
retrieval capabilities only when their required semantics are implemented.

Keep lifecycle handlers synchronous and bounded, with no detached mutation
worker. An API timeout can still leave a request executing remotely: prove retry
behavior around ambiguous writes and delayed completion. The API's lack of
atomic uniqueness remains a limitation for overlapping uncertain-outcome writes,
direct OSB callers, or concurrent external writers, rather than an unconditional
blocker for the agreed CAPI-driven MVP.

## 6. Headless builds differ between v1.6.0 and HEAD

At v1.6.0, `management/admin.rs:212` gates the entire UI/resource API router on
the `ui` feature. The app's `ui` feature embeds `ui/dist`. Building v1.6.0 without
that feature removes the management API needed by the broker.

Commit `50067185` (after v1.6.0) decouples API availability from embedded assets.
At inspected HEAD, `management/admin.rs:203-209` always merges the API router.
`crates/agentgateway-app/build.rs` and `src/lib.rs:14-17` allow an empty asset
directory. A fresh checkout can build without Node/UI assets and retain APIs.

**Decision:** Build from `main`, pinning the selected commit in the release
submodule for reproducibility. Use inspected `c0354ea426b25b4b3df357ba4b6afabcf4309660`
as the initial candidate. Omit embedded assets and the optional `ui` feature;
retain default crypto/allocator features and management APIs. Earlier planning's
unconditional "omit UI, retain API" assumption is incorrect for v1.6.0.

## 7. Offline source-build requirements

- `rust-toolchain.toml` pins Rust `1.99.0`, also at v1.6.0. The Rust compiler
  package version must match the tested source, not just the workspace MSRV.
- Build the app package explicitly (`agentgateway-app`, binary `agentgateway`)
  with release profile, locked dependencies, and offline mode.
- Cargo dependencies include pinned Git sources from GitHub and GitLab
  (`Cargo.toml:1-9`). Vendor those as well as registry crates, and include Cargo
  source-replacement configuration in release staging.
- Preserve required workspace crates, proto inputs, `catalog/`, and `tools/`.
  `crates/protos/build.rs` uses Rust `protox` compilation; it does not invoke a
  system `protoc` executable directly.
- Default features include native AWS-LC and allocator components. Inspect their
  vendored build scripts and prove C/C++ compiler, make/CMake, and runtime-library
  requirements on Noble. A Rust compiler blob alone does not prove build closure.
- `tools/report_build_info.sh` accepts `VERSION` and `GIT_REVISION`; set both when
  compiling release staging without Git metadata.
- The headless HEAD build avoids Node/pnpm. A UI-enabled v1.6.0 build needs prepared
  UI assets or a separately vendored offline Node/pnpm dependency set.
- Inspect the resulting executable's shared-library dependencies and package
  required runtime libraries and CA trust. No compilation VM network downloads.

## Next spike, in order

1. Choose/pin a source revision and prove offline headless build with resource API.
2. Run two gateway processes against one PostgreSQL database and a deterministic
   mock provider. Verify create/delete propagation, listener reconnection, scale-out
   preload, mixed-replica accounting, exhaustion convergence, and shutdown flush.
3. Exercise resource API metadata, raw-key/hash round trips, lost responses, and
   sequential identical/conflicting retries, including delayed write completion.
   Measure acknowledged versus active configuration separately.
4. Verify target CAPI lifecycle serialization and select credential recovery for
   the stateless Go broker; resolve binding/recreation semantics explicitly.
5. Package/deploy two gateways and singleton broker/PostgreSQL with BOSH DNS;
   verify app-container DNS resolution, TLS, reachability, and rolling updates.

Existing `architecture.md` and `implementation-plan.md` still describe the older
UAA/single-gateway design. This investigation records evidence for their next
revision; it does not claim those designs or a live deployment have been updated.
