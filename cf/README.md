# Deploying to Cloud Foundry

This bundle deploys the agentgateway inference app and its Open Service Broker
as separate Cloud Foundry apps. PostgreSQL stores gateway configuration and usage.

## Prerequisites

- CF CLI v8 access to a space where you can deploy apps, bind services, and
  create app-to-app network policies; broker registration requires the relevant
  operator permissions.
- A foundation with `cflinuxfs5`, the binary buildpack, container networking,
  an internal domain such as `apps.internal`, and routes for both apps.
- A PostgreSQL service binding with a `postgres://` or `postgresql://` URI in
  `uri` or `url`. Its database role must be able to create and update schemas.
- Provider connectivity and model pricing. For source builds, also install Docker
  and the tools described in the repository's development README.

## 1. Unpack the release

Download `cf-agentgateway-broker-linux-amd64.tar.gz` and `SHA256SUMS` from the
same GitHub release. In an empty directory:

```sh
sha256sum --check SHA256SUMS
tar -xzf cf-agentgateway-broker-linux-amd64.tar.gz
```

The archive contains this `README.md`, `manifest.yml`, `dev-vars.yml`, and the
tested binaries under `dist/`. Run the following commands from this directory.
For source builds instead, initialize submodules, run `devbox run vendor` and
`devbox run build` at the repository root, then run the deployment commands
from `cf/`.

## 2. Configure

`dev-vars.yml` contains non-secret values for the local lab, including its
DeepSeek-compatible endpoint. Copy it for your deployment and adjust every value
to match your foundation, provider, and service plan:

```sh
cp dev-vars.yml local-vars.yml
```

Add the following secret values to `local-vars.yml`; keep that file private:

```yaml
management_token: 'REPLACE_WITH_A_RANDOM_MANAGEMENT_TOKEN'
broker_password: 'REPLACE_WITH_A_DIFFERENT_RANDOM_BROKER_PASSWORD'
capsule_signing_seed: 'REPLACE_WITH_A_STABLE_32_BYTE_HEX_ED25519_SEED'
mcp_session_key: 'REPLACE_WITH_A_DIFFERENT_STABLE_32_BYTE_HEX_KEY'
trusted_ca: '' # Empty for system trust, or replace with the PEM issuer CA
```

For a PEM CA, use a YAML block scalar (for example `trusted_ca: |`) and indent
the complete certificate beneath it.

Provider credentials belong in the `gateway_models` JSON, if your provider
requires them. The file is passed to CF as environment configuration. Never
commit real credentials. In the repository, `cf/local-vars.yml` and
`cf/local-secrets.yml` are ignored. The lab's `cf/local-secrets.yml` contains
the existing credentials and can be used as a second vars file:
`--vars-file dev-vars.yml --vars-file local-secrets.yml`.

| Variables | What to set |
| --- | --- |
| `gateway_app_name`, `broker_app_name` | App names in the target space |
| `stack`, `buildpack` | CF stack and binary buildpack names |
| `gateway_memory`, `gateway_disk_quota`, `gateway_instances` | Gateway capacity and replica count |
| `broker_memory`, `broker_disk_quota`, `broker_instances` | Broker capacity and replica count |
| `gateway_route`, `broker_route` | Public app routes, without a scheme |
| `database_service_name` | Name of the existing PostgreSQL service instance to bind |
| `installation` | Unique broker installation identifier for shared gateway state |
| `management_url` | Private gateway management URL, including port 9090 |
| `management_token`, `broker_password` | Distinct strong credentials for management and OSB |
| `gateway_url` | Public inference URL including `https://` and `/v1` |
| `trusted_ca` | PEM CA for the public inference route, or an empty string |
| `gateway_models`, `model_catalog` | JSON strings for provider models and their pricing |
| `model_aliases`, `usd_allowance`, `budget_window` | Broker model allowlist and per-instance USD allowance |
| `mcp_servers` | JSON array of MCP backends; legacy entries use `name`, HTTP(S) `url`, optional literal `headers`; plan entries additionally configure stable `plan_id` and `auth`; see [federation](../docs/mcp-federation.md) |
| `mcp_url` | Public MCP URL, including `https://` and `/mcp` |
| `mcp_session_key` | Stable private 32-byte hexadecimal AES session key, shared across gateway replicas; required with MCP enabled |
| `capsule_signing_seed` | Stable private 32-byte Ed25519 seed encoded as 64 hex characters; shared by gateway sidecars |
| `capsule_witness_url`, `capsule_witness_public_key` | External witness URL and pinned hex Ed25519 public key; both empty disables delivery |

The aliases must match `gateway_models`, and catalog rates must correspond to
the provider's reported usage. The model catalog's prices are per million tokens.
The dev vars are lab examples, not defaults suitable for every foundation.

For example, set `mcp_servers` to a JSON array containing a server named
`shared-tools` with `url` pointing to its HTTP MCP endpoint. Every service instance
gets access to this global list. A Go CF sidecar authenticates MCP and writes
service-instance-attributed signed capsules and a checkpointed shared log into
the existing PostgreSQL database. The gateway allocation includes 128 MB reserved
for this sidecar; size `gateway_memory` for both processes. Generate and preserve
the signing seed before deployment. See the repository's `docs/mcp-capsules.md`
for storage, correlation, checkpointing, and failure semantics.

## 3. Deploy the apps

Target the intended org and space and create a PostgreSQL service if needed:

```sh
cf target -o YOUR_ORG -s YOUR_SPACE
cf marketplace
cf create-service POSTGRES_OFFERING POSTGRES_PLAN YOUR_DATABASE_SERVICE_NAME
```

Set `database_service_name` in `local-vars.yml` to that instance's name. Push
without starting, then map a private route and grant the broker network access:

```sh
cf push -f manifest.yml --vars-file local-vars.yml --no-start --redact-env
cf map-route YOUR_GATEWAY_APP apps.internal --hostname YOUR_MANAGEMENT_HOSTNAME
cf add-network-policy YOUR_BROKER_APP YOUR_GATEWAY_APP --protocol tcp --port 9090
cf start YOUR_GATEWAY_APP
cf start YOUR_BROKER_APP
```

Replace the names, internal domain, and hostname in those commands with your
configured values. `management_url` must point to the mapped internal route.
Ensure gateway-to-PostgreSQL and gateway-to-provider networking is allowed;
service binding does not necessarily create the required network policy.

For the local lab using its existing secrets file:

```sh
cf push -f manifest.yml --vars-file dev-vars.yml --vars-file local-secrets.yml --no-start --redact-env
cf map-route agentgateway apps.internal --hostname agentgateway-management
cf add-network-policy agentgateway-broker agentgateway --protocol tcp --port 9090
cf start agentgateway
cf start agentgateway-broker
```

Gorouter terminates public TLS and forwards inference to `$PORT`. Management
uses the private route on port 9090, protected by its own token. Raw admin/debug
remains loopback-only.

## 4. Register and use the broker

Register the broker at `https://YOUR_BROKER_ROUTE` using username `broker` and
the configured `broker_password`. Cloud Controller must trust the broker route's
TLS issuer and be able to reach it.

```sh
cf create-service-broker agentgateway broker https://YOUR_BROKER_ROUTE
cf enable-service-access agentgateway
cf create-service agentgateway shared inference
cf bind-service YOUR_APP inference
```

Enter the broker password at the prompt, or supply it through `CF_BROKER_PASSWORD`.

Restart or restage the consumer to load the binding. App bindings and service
keys return `url`, `api_key`, and `trusted_ca`. Clients send the key as a Bearer
token and use the PEM bundle for TLS verification, or system trust when empty.
Each instance has one key shared by its bindings: unbind preserves the key and
deprovision revokes it.

When `mcp_servers` is nonempty, bindings also return `mcp_url`. Use the same
Bearer key and trust CA for MCP. Tool recording fails closed; external witness
delivery happens in the background. MCP calls do not charge the inference budget.

## 5. Verify

- Check both apps with `cf apps` and inspect their logs if they fail to start.
- Create and bind a service instance, then call `/v1/models` and
  `/v1/chat/completions` using the binding credentials.
- Check that the broker can reach private gateway management and that the
  gateway can reach PostgreSQL and the provider.
- Restart or scale gateway instances and confirm existing credentials still work.

Budgets charge after responses and synchronize across replicas periodically.
Concurrent requests, streams and synchronization lag can overshoot an allowance.
Missing provider usage or catalog pricing can leave requests uncharged; verify
accounting with your chosen models before relying on it.
