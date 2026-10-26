# Gateway source findings

Inspected source: `c0354ea426b25b4b3df357ba4b6afabcf4309660`, pinned in
`src/agentgateway`. References below are relative to that revision.

## Headless management API

Commit `50067185` separates API availability from embedded UI assets. The pinned
revision's `management/admin.rs` always merges the resource API router, while
`agentgateway-app/build.rs` allows absent `ui/dist`. The gateway can therefore
build without Node/pnpm and still serve the management API.

## Replica propagation

`crates/agentgateway/src/config_store.rs` uses PostgreSQL LISTEN/NOTIFY. Writes
notify within their transaction; peers and reconnected listeners trigger reloads
through `state_manager.rs`. Resource write acknowledgement does not prove every
replica has applied the configuration. All replicas need compatible file baselines.

## Shared budgets

`http/budget/database.rs` flushes deltas every five seconds using additive SQL
upserts, then reads persisted totals and reconciles memory. Admission uses memory,
not transactional reservations. Runtime checks have demonstrated replica key
propagation, persisted usage and exhaustion convergence. This is eventual spend
accounting, not a strict prepaid ceiling or exactly-once accounting guarantee.

`http/apikey.rs` sets accounting identity from the key hash; `budget/mod.rs`
combines it with the budget name. Compatible reloads retain usage; rotating a key
changes identity. Graceful proxy termination attempts a final budget flush.

## Stateless broker resource model

Resource collection responses contain stored JSON, revisions and timestamps.
Raw `key` values are recoverable; `keyHash` is irreversible. Metadata supports
custom fields except the reserved `agentgateway.dev/` prefix. The broker uses
its own metadata namespace to resolve CF instances and compare fingerprints.

API-key creation assigns random resource UUIDs; updates require an existing key.
There is no native metadata selector, item GET or expected-revision write.
The broker lists and filters resources, relies on CAPI serialization, and rejects
duplicate matches. Bindings share the instance credential; there is no mutable
binding map. Concurrent external writers and delayed uncertain-outcome creates
would require stronger gateway API primitives.

## Source build

Rust is pinned to 1.99.0. Vendor Cargo registry and Git sources, preserve workspace
crates/proto inputs/catalog/tools, and build `agentgateway-app` explicitly.
`crates/protos/build.rs` uses Rust protox, not an external protoc executable.
Native crypto/allocator dependencies require C/C++ build tools. The Docker image
provides these; libgcc is copied alongside the gateway for runtime use.
Set VERSION/GIT_REVISION because build staging need not include Git metadata.

The CF build uses a one-job unoptimized release profile to fit the development
VM's memory. Runtime artifacts have been tested on cflinuxfs5. See
[CF verification](../cf/verification.md) for deployment evidence.
