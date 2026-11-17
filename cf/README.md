# Deploying to Cloud Foundry

This bundle deploys the agentgateway inference app and its Open Service Broker
as separate Cloud Foundry apps. PostgreSQL stores gateway configuration and usage.

## Prerequisites

- A CF CLI with `map-route --app-port` and `add-route-policy`, plus Python 3,
  access to deploy apps and bind services; broker registration requires the relevant
  operator permissions.
- A foundation with `cflinuxfs5`, the binary buildpack, container networking,
  an mTLS domain such as `apps.identity` with route-policy enforcement enabled,
  and routes for both apps. The identity domain must default-deny without policies.
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

The archive contains this `README.md`, `configuration.md`, `manifest.yml`, `deploy`,
`dev-vars.yml`, and the tested binaries under `dist/`. Run the following commands
from this directory.
For source builds instead, initialize submodules, run `devbox run vendor` and
`devbox run build` at the repository root, then run the deployment commands
from `cf/`.

## 2. Configure

### Application YAML configuration

Keep deployment variables (app names, routes, sizing, database binding) in your
vars file (start with `deployment.example.yml`). Copy `broker.example.yml` to private `broker.yml` and
`gateway.example.yml` to private `gateway.yml`. These files contain native YAML
lists/maps rather than JSON strings. Replace all example credentials, identities,
resource names and illustrative prices. See [configuration](configuration.md).

Install `requirements.txt` in a Python virtual environment. In Bash, validate
conversion in checked commands **before** using process substitution (failures
inside process substitution do not automatically fail the outer command):

```bash
set -e
python3 config-vars --check broker.yml gateway.yml
python3 deploy YOUR_GATEWAY_APP YOUR_BROKER_APP apps.identity YOUR_MANAGEMENT_HOSTNAME \
  --vars-file deployment-vars.yml \
  --vars-file <(python3 config-vars broker_config broker.yml) \
  --vars-file <(python3 config-vars gateway_config gateway.yml)
```

Run the conversion preflight with shell `set -e` or connect the commands with
`&&`. The manifest places one JSON string in `AGENTGATEWAY_CONFIG` on each app.
The sidecar reads its subsection independently. Backend configuration belongs to
the gateway app; the broker has no database binding.

MCP client identity is selected at instance creation, for example
`cf create-service development-tools standard tools -c '{"client_id":"YOUR-APPLICATION-CLIENT-ID"}'`.
Configure the external principal's federation trust and upstream permissions
separately. The release neither creates that principal nor requires its secret.

Existing instance-free deployments can replace the catalog directly, then run
`cf update-service-broker` and enable access to the intended offerings. Do not use
this as an automatic migration of existing instance identities. The helper
removes retired environment settings before restarting; keep cryptographic keys
and installation identity stable.

The older configuration instructions below describe the legacy runtime format;
the new manifest uses the consolidated placeholders shown above.

See the [complete configuration reference](configuration.md) for every vars-file
and environment name, required/default values, nested JSON fields, and examples.

`dev-vars.yml` contains non-secret values for the local lab, including its
DeepSeek-compatible endpoint. Copy it for your deployment and adjust every value
to match your foundation, provider, and service plan:

```sh
cp dev-vars.yml local-vars.yml
```

Add the following secret values to `local-vars.yml`; keep that file private:

```yaml
broker_password: 'REPLACE_WITH_A_RANDOM_BROKER_PASSWORD'
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
| `management_url` | HTTPS identity route URL, normally on port 443; destination port is 9090 |
| `management_trusted_ca` | Optional management CA PEM; empty uses system and CF platform trust |
| `broker_password` | Strong credential for OSB |
| `gateway_url` | Public inference URL including `https://` and `/v1` |
| `trusted_ca` | PEM CA for the public inference route, or an empty string |
| `gateway_models`, `model_catalog` | JSON strings for provider models and their pricing |
| `inference_plans` | JSON array of operator-defined inference plans: `id`, `name`, `description`, `free`, `models`, `budget`, `window`; `[]` publishes no inference plans |
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

Set `database_service_name` in `local-vars.yml` to that instance's name. Run the
deployment helper from the bundle directory (or `cf/` in a source checkout):

```sh
python3 deploy YOUR_GATEWAY_APP YOUR_BROKER_APP apps.identity YOUR_MANAGEMENT_HOSTNAME --vars-file local-vars.yml
```

Replace the names, identity domain, and hostname with your configured values.
`management_url` must be `https://YOUR_MANAGEMENT_HOSTNAME.apps.identity`.
The helper maps port 9090 before replacing gateway containers, grants only the
broker app identity access, removes obsolete token environment variables, and
rolls both apps. It refuses unexpected grants on an existing management route.
Uploads use `--no-start`, so this helper stops existing apps during deployment.
If the broker app is deleted and recreated, remove the old app-GUID policy and
rerun the helper to grant the new app identity.
Ensure gateway-to-PostgreSQL and gateway-to-provider networking is allowed;
service binding does not necessarily create the required network policy.

For the local lab using its existing secrets file:

```sh
python3 deploy agentgateway agentgateway-broker apps.identity agentgateway-management --vars-file dev-vars.yml --vars-file local-secrets.yml
```

Gorouter terminates public TLS and forwards inference to `$PORT`. Management
uses the identity route, with mTLS and a broker-only route policy, forwarding to
port 9090. The broker loads CF instance identity on each TLS handshake. The
launcher trusts certificates in `CF_SYSTEM_CERT_PATH` and optional
`MANAGEMENT_TRUSTED_CA`. Raw admin/debug remains loopback-only. `RUST_MIN_STACK`
is fixed by the manifest at `16777216`; it is not an operator variable.

### Migrating an existing token-based deployment

Before deploying the tokenless gateway, remove its old management internal route
and broker-to-gateway network policy. This temporarily makes OSB operations
unavailable until deployment completes. Audit other routes and network policies
to ensure only the identity route can reach management port 9090.

```sh
cf unmap-route YOUR_GATEWAY_APP apps.internal --hostname YOUR_OLD_MANAGEMENT_HOSTNAME
cf remove-network-policy YOUR_BROKER_APP YOUR_GATEWAY_APP --protocol tcp --port 9090
```

Remove `management_token` from private vars/secrets files and set the new
`management_url` and `management_trusted_ca`, then run the helper above.

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
- Check the broker's `/ready` endpoint returns 200 through identity-domain
  management, and that the gateway can reach PostgreSQL and the provider.
- Confirm `cf route-policies` allows only the broker app on the management route,
  and that neither app has `MANAGEMENT_TOKEN`. Unauthenticated callers and other
  app identities must be denied management access; public inference routes must
  not expose `/api/runtime` or `/api/config/resources/llm.apiKey`.
- Restart or scale gateway instances and confirm existing credentials still work.

Budgets charge after responses and synchronize across replicas periodically.
Concurrent requests, streams and synchronization lag can overshoot an allowance.
Missing provider usage or catalog pricing can leave requests uncharged; verify
accounting with your chosen models before relying on it.
