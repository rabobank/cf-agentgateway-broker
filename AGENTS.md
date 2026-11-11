# Architecture constraints

## The service broker is a stateless OSB-to-gateway translator

The broker translates Open Service Broker lifecycle requests into agentgateway
configuration through the gateway's management API. **Agentgateway's managed
resources are authoritative for durable service-instance and binding state.**

- Keep the service catalog and available backend/auth-method definitions in
  operator configuration. Generate the OSB catalog from that configuration.
- Represent developer-selected instance parameters, binding grants and lifecycle
  metadata as gateway-managed configuration resources, recoverable through the
  management API after broker restart or on another broker replica.
- Do not give the broker a direct PostgreSQL connection or database service
  binding. Do not introduce broker-owned tables, schema creation/migrations,
  database locks, or a separate authoritative persistence layer.
- Do not write gateway tables directly from the broker. Use supported management
  APIs; extend the gateway resource model when the necessary configuration
  primitive is missing rather than bypassing it with broker database state.
- Broker-local memory may hold request-scoped data and disposable caches, but
  must not be required for correctness, recovery or revocation.
- Keep lifecycle operations idempotent under repeated requests and uncertain
  management-write outcomes. Support orphan cleanup even when a prior create or
  bind did not return successfully. Do not assume CAPI indefinitely retries the
  same failed bind: it can request orphan unbind and allocate a new binding GUID.
- Keep stable resource IDs distinct from generation-specific backend/session
  identities where needed. Preserve identity isolation and fail-closed access
  during retries, revocation and replica configuration propagation.

PostgreSQL may persist agentgateway configuration and capsule/checkpoint data as
part of the **gateway fabric**. That does not make database ownership part of the
broker's responsibilities. The capsule sidecar's ledger and authorization role
must not be used to introduce a second broker-owned grant store.

When changing provisioning, binding or authorization, verify restart recovery,
idempotent retries, orphan cleanup and revocation against gateway-owned state.
Flag any existing code that violates this boundary and migrate it; do not treat
the presence of broker database code as an architectural precedent.

## Provider-neutral MCP integration

Operators configure MCP endpoints, authentication methods, token authorities,
audiences, scopes and permitted identity parameters. Do not hard-code a particular
cloud provider, hosted MCP service, tenant or resource ID into broker behavior.
Capsules attribute the selected backend identity using provider-neutral fields;
selection is not proof of successful upstream authentication. Never record
tokens, assertions, secrets or credential-derived fingerprints in capsules.
