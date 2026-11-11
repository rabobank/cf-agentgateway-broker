# CF architecture

Plan-based MCP access extends the lifecycle below with gateway-managed instance
and binding resources, service-account JWT authorization, generic backend auth, and
per-binding routes. See [MCP federation](mcp-federation.md) for its configuration,
revocation and selected-identity capsule contract. The inference lifecycle and
legacy global MCP entries retain the shared-key model described here.

```text
Cloud Controller -> CF HTTPS route -> stateless Go OSB broker
                                         |
                              private apps.internal:9090
                                         |
CF consumers -> CF HTTPS route -> agentgateway replicas
                                         |
                              PostgreSQL service binding
                                         |
                                 PostgreSQL service

agentgateway replicas -> operator-configured model providers

CF MCP consumers -> gateway /mcp -> operator-configured shared MCP servers
                         |
                 loopback Go CF sidecar
                         |
          capsules + shared CLL in PostgreSQL
                         |
           elected worker -> external witness
```

The gateway's API resources are authoritative. The broker manages them through the
gateway API; gateway replicas persist configuration and usage in PostgreSQL.
Raw admin/debug stays loopback-only; the management listener
requires a separate operator token and app-to-app network policy. It is not
attached to the public inference route.

## Lifecycle

Each instance maps to a random gateway key resource. Metadata carries installation
identity, CF instance UUID, org/space, service/plan IDs and provision fingerprint.
Provision lists resources, checks ownership/fingerprint, and creates a key if absent.
Bind/service-key returns the shared key plus inference URL and issuer CA bundle.
Unbind leaves the shared key intact; deprovision deletes the resource.

Lifecycle operations are synchronous and rely on CAPI serialization of same-instance
requests. OSB 2.15 and newer compatible 2.x are accepted. No custom parameters,
plan updates, async workflows or retrieval capabilities are advertised. Delayed
remote writes with uncertain outcomes remain a limitation of the gateway API's
lack of atomic create/CAS support.

## State and accounting

PostgreSQL LISTEN/NOTIFY drives configuration reloads. Every five seconds, gateway
replicas flush additive usage deltas and reconcile shared totals. Admission checks
memory; post-response charges, streams, concurrent requests and replica lag can
overshoot. Crashes can lose unflushed usage. Missing provider usage/pricing can leave
requests uncharged. Windows are epoch-aligned; budget identity is key hash plus
budget name. Keep key/hash, budget name, unit and window stable during reconciliation.

PostgreSQL durability determines recovery of instance credentials, configuration
and accounted usage. Back up this state according to the deployment's recovery
requirements. An ephemeral database is suitable for development but loses state
when its backing instance is replaced.

## Build and configuration

The pinned gateway source compiles without UI assets in a pinned Docker image.
The binary buildpack runs it on cflinuxfs5. A Go launcher generates protected
runtime configuration from environment settings and PostgreSQL VCAP_SERVICES.
Gorouter terminates public TLS; bindings supply its issuer CA to consumers.

See [deployment](../cf/README.md) and [source findings](source-investigation.md).
See [MCP and capsules](mcp-capsules.md) for global MCP settings, service-instance
attribution, transactional recording, and PostgreSQL-elected checkpoint workers.
