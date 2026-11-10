# MCP service plans and federated backend authentication

Operators can publish one plan per configured HTTP MCP backend through the existing
`agentgateway` service. Inference retains its existing `shared` plan. A backend
with `plan_id` is exposed only through broker-created app-binding routes, never
through the legacy global `/mcp` target list.

## Operator configuration

`mcp_servers` / `MCP_SERVERS` accepts entries with `name`, stable `plan_id`,
`description`, `url`, optional static `headers`, and `auth`:

- `method`: `none`, `static`, `passthrough`, or `federated_client_credentials`.
- `issuer`: exact trusted JWT issuer.
- `jwks_url`: HTTPS signing-key endpoint.
- `audience`: required incoming JWT audience, returned in binding credentials.
- `identity_reference`: optional non-secret operator label for selected-identity
  capsule attribution.

All plan modes require a valid caller JWT and live service-account binding.
`none` strips caller authorization; `static` replaces it with an explicitly
configured Authorization header; `passthrough` forwards the validated token.

Federated client credentials additionally requires:

- `token_endpoint`: HTTPS URL with a fixed authority and optional path placeholders.
- `scopes`: nonempty list of requested resource scopes.
- `parameters`: mapping of placeholder names to full-match validation regexes.

For example, a provider-neutral token endpoint may be
`https://identity.example/{tenant}/oauth/token`, with `tenant` validated by
`[a-z0-9-]+`. Instance creation supplies `client_id` and `tenant`. Unknown
parameters, authority substitution, and path/query injection are rejected.
Endpoints, scopes, audiences and headers are operator configuration; no cloud
resource IDs or provider-specific MCP behavior are built into the implementation.

## Developer lifecycle

Configure the external identity's federation trust and resource permissions out
of band. The broker neither provisions cloud identities nor stores their secrets.

Create an instance using the configured plan. For federation, pass `client_id`
and the operator-defined endpoint parameters as OSB creation parameters. Other
auth modes accept no instance parameters. The backend configuration is snapshotted
at creation: changing operator configuration affects new instances, not existing
grants. Remove a plan only after its instances are deleted.

Assign the CF service account **before** binding. The broker requires an app
binding with trusted CF `context.service_account.{issuer,subject}` matching the
configured issuer. It rejects service keys. Binding parameters and plan updates
are not supported.

Credentials contain `mcp_url`, `audience`, `issuer`, `subject`, `auth_method`, and
`trusted_ca`. There is no instance API key for plan-based access. The MCP URL is
binding-specific. The app obtains an audience-targeted JWT from UAA using its
instance certificate and sends it as the Bearer token on each MCP HTTP request.
Service-account subjects are matched exactly, including their org prefix; they
are not reconstructed from names.

## Runtime and persistence

The gateway validates signature, issuer, audience, required expiry/subject, and
time claims using its native JWT policy. Authorization checks the selected
account subject. A loopback extAuthz call reads the current grant from PostgreSQL
for every HTTP request, including session streams and deletion. This endpoint is
specifically a federation delegation endpoint accepting the configured exchange
audience, not an unrestricted gateway bearer-token audience.

Native backend auth sends the original JWT to the configured token endpoint as
`client_assertion`, with `grant_type=client_credentials`, JWT-bearer assertion type,
the instance's selected `client_id`, and configured `scope`. The upstream receives
only the exchanged bearer token. No Azure SDK, secret, or provider-specific flow
is involved.

Token caches are bounded, replica-local and policy-scoped, keyed by assertion
digest, with concurrent exchanges coalesced. Expiry is capped by the assertion's
expiry and token endpoint lifetime with a safety margin. Tokens and assertions
are not stored in PostgreSQL. Each binding has a distinct MCP target name so
session state cannot be moved to another binding's backend. Random instance and
binding generations make target identities distinct after deletion/recreation;
the live grant check rejects stale route generations.

Broker-owned `agw_mcp_instances` and `agw_mcp_bindings` tables persist grants,
instance parameters and backend snapshots. Instance operations are serialized by
PostgreSQL advisory session locks, retained across grant commit and route cleanup.
Gateway routes are written through the
management API and persisted in its normal config store. Bind writes a route
before committing its grant, so pre-commit requests fail closed. Route resource
IDs depend only on installation, instance ID and binding ID; target names also
include the random instance/binding generations. A retry after a lost management
response or failed grant commit overwrites the same route instead of creating a
competing route for the binding URL. Orphan unbind can derive the route ID and
delete it even if no grant was committed or the instance is already absent.
Unbind and
deprovision commit revoked tombstones before route cleanup; retries finish cleanup
without reauthorizing the identity. No cross-store atomic transaction is claimed.
Back up these grant tables with gateway/capsule state.

Revocation prevents new requests even on replicas retaining an old route. It does
not cancel an already executing tool or revoke issued provider tokens. Already
open streams are not proactively disconnected. Frontend JWKS refresh follows the
gateway's resource-loader behavior; coordinate signing-key overlap accordingly.

## Selected-identity capsules

All plan auth modes record a provider-neutral `selected_identity`: backend ID,
auth method, optional identity reference, and known client ID/token authority.
The plan also records the service account and binding. Outcomes preserve the
planned identity, rather than looking up potentially changed configuration.
This is **selection attribution**, not proof the upstream authenticated that
principal. No tokens, assertions, secrets or credential-derived fingerprints are
included. Legacy global MCP entries retain their existing instance attribution.

## Build and verification

The agentgateway submodule points to the `rkoster/agentgateway` fork on
`feat/federated-client-credentials`; its exact commit is pinned in
`scripts/agentgateway-version`. The fork's tests commit precedes its implementation.
The native grant is available through local/management JSON configuration; xDS
and Kubernetes controller APIs are not extended in this initial delivery.

The current unoptimized release profile needs `RUST_MIN_STACK=16777216` for the
combined JWT/authorization/federation policy path; the CF manifest sets it.

Run `devbox run test` for PostgreSQL lifecycle, race checks, native-gateway signed
JWT/federation/MCP integration, revocation, session isolation and capsule tests.
The integration uses local signing keys and mock OAuth/MCP endpoints, not real
providers. The ordinary packaged acceptance suite remains `devbox run acceptance`.
