# Configuration reference

This reference covers the CF manifest, launcher, broker and capsule sidecar.
Gateway provider/policy formats follow the agentgateway revision pinned in
`scripts/agentgateway-version`. The broker adds no provider-specific defaults.
For the complete upstream nested schemas at the current pin, see
[model/provider configuration](https://github.com/rkoster/agentgateway/blob/59452e8176daa21db3f1c2b8f240d55a63e4d8f9/crates/agentgateway/src/types/local.rs)
and [catalog types](https://github.com/rkoster/agentgateway/blob/59452e8176daa21db3f1c2b8f240d55a63e4d8f9/crates/agentgateway/src/llm/catalog/model.rs).

## Supplying values

Lowercase names are `manifest.yml` placeholders supplied with
`cf push --vars-file local-vars.yml --redact-env`. Uppercase names are runtime
environment variables. Supply every manifest placeholder, even when empty.
`dev-vars.yml` contains lab examples, not runtime defaults. CF supplies `PORT`
and service-binding JSON automatically.

JSON settings must be **strings containing JSON**, not native YAML arrays/maps.
Use folded YAML scalars (`>-`) for JSON and literal scalars (`|`) for PEM:

```yaml
inference_plans: >-
  [{"id":"inference-basic","name":"basic","description":"Shared inference allowance","free":true,"models":["chat-small"],"budget":10,"window":"24h"}]
mcp_servers: '[]'
trusted_ca: ''
```

Inside JSON, use booleans and numeric plan budgets, not quoted versions.
Pricing rates are the exception: they are decimal strings. Empty lists are `[]`,
not `null`. Keep tokens, provider credentials, static headers, signing seeds and
session keys in private vars files. Trust certificates and witness public keys
are public data. Keep installation identities and replica-shared keys stable.

## CF deployment settings

| Vars-file name | Format / purpose |
| --- | --- |
| `gateway_app_name`, `broker_app_name` | CF app-name strings in the target space. |
| `stack`, `buildpack` | Installed CF stack/buildpack names; binaries are built for `cflinuxfs5` and the binary buildpack. |
| `gateway_memory`, `broker_memory` | CF memory strings, e.g. `768M`, `128M`, `1G`. Gateway memory includes the 128 MB sidecar reservation. |
| `gateway_disk_quota`, `broker_disk_quota` | CF disk quota strings, e.g. `1G`. |
| `gateway_instances`, `broker_instances` | Positive integer replica counts for a running deployment. |
| `gateway_route`, `broker_route` | Route strings, e.g. `gateway.apps.example`, without a scheme. |

CF interprets these settings. Capacity and installed stack/buildpack names depend
on the foundation.

## Runtime variables and manifest mappings

“Required” means nonempty unless stated otherwise. The gateway and sidecar share
a PostgreSQL binding. The broker has no database binding or connection.

| Vars-file name | Environment variable | Process | Format, requirement and default |
| --- | --- | --- | --- |
| Fixed by manifest | `APP_ROLE` | Launcher | Required: exactly `broker` or `gateway`. |
| CF supplied | `PORT` | Launcher | Required decimal integer, `1..65535`. Broker/inference listens on `0.0.0.0:$PORT`. |
| `database_service_name` | `DATABASE_SERVICE_NAME` | Gateway, sidecar | Exact bound service-instance name. Empty runtime value defaults to `agentgateway-state`. |
| CF supplied | `VCAP_SERVICES` | Gateway, sidecar | JSON service-binding object; see [database binding](#database-binding). Required by gateway launcher. |
| `installation` | `INSTALLATION` | Broker | Required stable nonempty namespace for this installation's gateway-owned resources. |
| `installation` | `CAPSULE_LOG_ID` | Sidecar | Required stable shared producer-log identity. Manifest uses the same value as `INSTALLATION`. |
| `broker_password` | `BROKER_PASSWORD` | Broker | Required secret string. Launcher fixes OSB Basic-auth username to `broker`. |
| `management_token` | `MANAGEMENT_TOKEN` | Gateway, broker | Required secret string, identical on both apps; broker sends it as a Bearer token. |
| `management_url` | `MANAGEMENT_URL` | Broker | Required HTTP(S) URL with host, no userinfo/query/fragment. Typically `http://gateway-management.apps.internal:9090`. Resource paths are appended to this base. |
| `gateway_url` | `GATEWAY_URL` | Broker | Required HTTP(S) URL with host, no userinfo/query/fragment. Public inference base, typically `https://gateway.apps.example/v1`; returned in inference bindings. |
| `trusted_ca` | `GATEWAY_TRUSTED_CA` | Broker | Optional PEM consumer trust bundle. Empty means consumer system trust; does not configure management TLS trust. |
| `inference_plans` | `INFERENCE_PLANS` | Broker | Required JSON array; `[]` disables inference plans. See [inference plans](#inference-plans). |
| `gateway_models` | `GATEWAY_MODELS` | Gateway | Required JSON string containing gateway `llm.models` array. See [gateway models](#gateway-models). |
| `model_catalog` | `MODEL_CATALOG` | Gateway | Required JSON string containing pricing catalog object. See [pricing](#model-catalog-and-pricing). |
| `mcp_servers` | `MCP_SERVERS` | Gateway, broker | Backend JSON array. Missing/empty runtime value means `[]`; explicitly set `[]` in manifest deployments. Same definitions on both apps. |
| `mcp_url` | `MCP_URL` | Broker | Required with nonempty MCP backend list. HTTP(S) URL with host, no userinfo/query/fragment; normally `https://gateway.apps.example/mcp`. Ignored by launcher when MCP is disabled. |
| `mcp_session_key` | `SESSION_KEY` | Gateway | Required with MCP enabled: 32 bytes as 64 hex characters, no `0x`. Stable AES-256-GCM key shared across replicas; rotation invalidates sessions. Separate from signing seed. |
| `capsule_signing_seed` | `CAPSULE_SIGNING_SEED` | Sidecar | Required even with MCP disabled in this manifest: 32-byte Ed25519 seed as 64 hex characters. Stable/shared across sidecars; rotation requires coordinated trust/checkpoint handling. |
| `capsule_witness_url` | `CAPSULE_WITNESS_URL` | Sidecar | Optional witness base URL; submits `POST /checkpoints`. Empty disables delivery, not signed checkpoint creation. |
| `capsule_witness_public_key` | `CAPSULE_WITNESS_PUBLIC_KEY` | Sidecar | Required with witness URL: 32-byte Ed25519 public key as 64 hex characters. Set both witness values to `''` to disable delivery. |
| Fixed by manifest | `RUST_MIN_STACK` | Gateway | `16777216` bytes for the pinned gateway's combined JWT/authorization/federation path. |

Hexadecimal keys may use upper/lowercase digits. The launcher writes runtime JSON,
catalog JSON and inference CA PEM under `config/` with mode `0600`, and sets
`LD_LIBRARY_PATH=lib` before executing the binary. Management listens on `9090`;
admin/debug stays on `127.0.0.1:15000`.

### Sidecar overrides outside the manifest

| Environment variable | Format / default |
| --- | --- |
| `DATABASE_URL` | Optional PostgreSQL connection string; overrides sidecar binding lookup only, not the gateway launcher. |
| `CAPSULE_GRPC_ADDR` | TCP listen address, default `127.0.0.1:50051`; must match gateway ExtMcp configuration. Keep loopback-only. |
| `CAPSULE_AUTH_ADDR` | TCP listen address, default `127.0.0.1:50052`; must match gateway extAuthz configuration. Keep loopback-only. |
| `CAPSULE_CHECKPOINT_ENTRIES` | Optional unsigned decimal 64-bit checkpoint entry count, also subject to upstream runner validation. Normal cadence is 100 records or 900 seconds; polling every 5 seconds. |

### Database binding

The launcher searches all service-label groups for the exact configured `name`.
It selects string `credentials.uri` first, then `credentials.url` if `uri` is not
a string. An invalid string `uri` fails rather than falling back to `url`.
Scheme must be `postgres`/`postgresql`, with a host. Userinfo and query connection
options are permitted here. Configure TLS according to your database service.

```json
{
  "postgresql": [
    {"name":"gateway-state","credentials":{"uri":"postgresql://USER:PASSWORD@db.example:5432/gateway?sslmode=require"}}
  ]
}
```

Gateway configuration/lifecycle resources and capsule/checkpoint evidence are
durable gateway-fabric state. The sidecar reads gateway-owned authorization
resources, not a parallel broker grant store. Back up the gateway and capsule
tables together. Provisioning privileges and storage semantics are documented
in the deployment guide and capsule documentation.

## Inference plans

`inference_plans` / `INFERENCE_PLANS` is an array of these objects:

| Field | JSON type | Rules |
| --- | --- | --- |
| `id` | String | Required stable OSB ID matching `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`; UUIDs work. |
| `name` | String | Required OSB name with the same identifier rules. |
| `description` | String | Required; not blank or whitespace-only. |
| `free` | Boolean | Optional, defaults `false`; OSB metadata, not a switch disabling accounting. |
| `models` | Array of strings | Required nonempty explicit aliases matching gateway model names; empty values and `*` wildcards rejected. |
| `budget` | Number | Required positive finite USD allowance, e.g. `10` or `1.25`, shared by one instance's bindings. |
| `window` | String | Required duration, e.g. `1h`, `24h`, `30d`. Broker checks nonempty; gateway parses/validates on publication. |

All inference and plan-enabled MCP IDs/names must be unique across the catalog.
At least one inference or MCP plan is required; legacy targets are not plans.
No name is reserved. Keep IDs/installation stable; remove plans only after deleting
their instances. Changes affect new provisions, not existing gateway-owned
policies. `MODEL_ALIASES`, `USD_ALLOWANCE`, `BUDGET_WINDOW` are no longer used.

Despite the gateway field name `rolling`, windows are fixed and Unix-epoch aligned:
`1h` follows UTC hours, `24h` starts at midnight UTC, and `30d` is consecutive
30-day periods, not calendar months. Post-response accounting, concurrency and
replica synchronization can overshoot. Missing pricing/usage can leave calls
uncharged. Inference unbind preserves the shared key; deprovision revokes it.

## Gateway models

`gateway_models` / `GATEWAY_MODELS` is passed unchanged as gateway `llm.models`.
The launcher checks JSON syntax; the pinned gateway validates the schema.

```json
[
  {
    "name": "chat-small",
    "provider": "openai",
    "params": {
      "model": "upstream-chat-small",
      "baseUrl": "https://provider.example/v1",
      "apiKey": "REPLACE_WITH_PROVIDER_KEY"
    }
  }
]
```

| Field | JSON type / meaning |
| --- | --- |
| `name` | Required client-facing model match string. Use explicit aliases in broker plans. |
| `provider` | Required selector. Built-in strings: `openai` (`openAI` alias), `anthropic`, `gemini`, `vertex`, `bedrock`, `azure`, `copilot`. Inline custom objects and preset strings are also supported; provider references are not supported by this launcher (see below). |
| `params` | Optional object, defaults `{}`; connection/model parameters below. |
| `id` | Optional stable string identity for this model entry. |
| `visibility` | Optional `public` (default) or `internal`; controls direct client access. |

Inline custom configuration (`"provider": {"custom": {...}}`) and upstream preset
strings such as `"mistral"` or `"ollama"` are self-contained model selectors; their
formats and defaults follow the pinned gateway's
[custom-provider schema](https://github.com/rkoster/agentgateway/blob/59452e8176daa21db3f1c2b8f240d55a63e4d8f9/crates/llm/src/custom.rs).

Upstream also supports `"provider": {"reference": "provider-name"}`, but it
requires a matching named entry in `llm.providers`. The CF launcher emits only
`llm.models` and exposes no `llm.providers` setting, so reference selectors in
`GATEWAY_MODELS` fail startup with `references unknown provider`. Use a built-in,
preset or inline custom selector here. Named references require a separately
authored full gateway configuration instead of this launcher's environment format.

| `params` field | JSON type / meaning |
| --- | --- |
| `model` | Optional upstream model-ID string replacing the alias. Defaults to incoming model. |
| `baseUrl` | Optional HTTP(S) upstream base. Provider endpoint paths are appended: OpenAI-compatible `/v1` produces `/v1/chat/completions`. |
| `apiKey` | Optional secret string or upstream watched-file form. Omitted credentials follow provider environment detection; explicit configuration is preferable here. |
| `awsRegion` | Optional Bedrock region string. |
| `bedrockEndpointPreference` | `runtimePreferred` (default), `mantlePreferred`, `mantleOnly` or `runtimeOnly`. |
| `vertexRegion`, `vertexProject` | Vertex region/project strings. |
| `azureResourceName`, `azureResourceType`, `azureApiVersion`, `azureProjectName` | Azure settings; type `openAI` or `foundry`; Foundry requires project. |
| `tokenize` | Optional boolean, default `false`. |
| `hostOverride`, `pathOverride`, `pathPrefix` | Deprecated connection overrides; prefer `baseUrl`. |

Optional upstream policy fields: `defaults`/`overrides` are JSON maps;
`transformation`/`finalTransformation` map payload fields to CEL strings;
`requestTransformation`, `requestHeaders`, `responseHeaders`, `tls` (alias
`backendTLS`), `auth`, `health`, `backendTunnel`, `guardrails`, `promptCaching`,
`matches` follow gateway policy schemas. `passthrough` is `detect` or `opaque`.
For the complete advanced nested formats, use the pinned gateway's
`LocalLLMModels`/`LocalLLMParams` definitions, rather than a different release's docs.

## Model catalog and pricing

`model_catalog` / `MODEL_CATALOG` is separate from gateway models and OSB catalog.
Hierarchy: `providers.<canonical-provider>.models.<upstream-model-id>`. These are
upstream IDs, not necessarily client aliases. An OpenAI-compatible endpoint uses
provider key `openai` even when hosted elsewhere.

```json
{
  "providers": {
    "openai": {
      "models": {
        "upstream-chat-small": {
          "rates": {"input":"0.25","output":"1.00","cacheRead":"0.05"}
        }
      }
    }
  }
}
```

Prices are **nonnegative decimal strings**, at most six fractional digits.
`"0"` means zero-priced; omission means unknown. Optional
`metadata.generatedAt` is an RFC 3339 timestamp.

| `rates` field | Unit / fallback |
| --- | --- |
| `input`, `output` | USD per million prompt/completion tokens. |
| `cacheRead`, `cacheWrite` | USD per million cache-read/cache-write tokens. |
| `cacheWriteDetails` | Map of TTL strings (e.g. `1h`, `12h`) to rates; omitted TTL falls back to `cacheWrite`. |
| `reasoning` | USD per million reasoning tokens; falls back to `output`. |
| `inputAudio`, `outputAudio` | USD per million audio tokens; fall back to `input`/`output`. |
| `perPage` | USD per document/OCR page. |
| `perAudioMinute` | USD per minute of input audio. |

Optional `tiers` is an array of objects with required `rates`, unsigned
`contextOver` (default `0`), and optional `serviceTier` (omission matches any).
Rates overlay base rates; context thresholds must increase within each service
tier. `serviceTier` is `standard`, `flex`, `priority` or `reserved`; gateway
normalizes served provider tier labels to these values.

Optional model metadata: string `name`, `status`, `released`; string-array `tags`;
unsigned token counts `limits.{context,input,output}`; string arrays
`modalities.{input,output}`; booleans
`capabilities.{reasoning,structuredOutput,toolCall,temperature}`;
`reasoning.toggle` boolean, `reasoning.effort` string array and
`reasoning.budgetTokens.{min,max}` unsigned counts. Omitted capabilities are
unknown. Gateway validates catalog fields. Verify rates against provider usage.

## MCP backends and authentication

`mcp_servers` / `MCP_SERVERS` is an array of backend objects:

| Field | JSON type | Rules |
| --- | --- | --- |
| `name` | String | Required unique identifier using plan-ID rules. Also OSB name with `plan_id`. |
| `url` | String | Required HTTP(S) MCP endpoint with host; no userinfo/fragment. Queries currently allowed here. Plan endpoints must use HTTPS. |
| `plan_id` | String | Optional stable OSB identifier; omitted/empty means legacy global target. |
| `description` | String | Optional. Blank/whitespace defaults to `Access to the <name> MCP backend`. |
| `headers` | Object of strings | Optional literal upstream headers, not CEL. Names must be HTTP tokens; values reject control characters except tab. |
| `auth` | Object | Plan settings below. Legacy entries must omit `auth.method`; nonempty method requires `plan_id`. |

Legacy entries share global `/mcp`, authenticated by inference-instance key.
Literal Authorization header supplies upstream credentials; otherwise caller
authorization is stripped. Legacy targets do not publish OSB plans.

```json
[{"name":"shared-tools","url":"https://tools.example/mcp","headers":{"X-API-Key":"REPLACE_WITH_UPSTREAM_KEY"}}]
```

Plan entries are accessible only through binding-specific routes. All modes
require trusted incoming JWTs and live app service-account bindings. `none` means
no **upstream** auth, not unauthenticated consumer access.

| `auth` field | JSON type | Rules |
| --- | --- | --- |
| `method` | String | Required for plans: `none`, `static`, `passthrough`, `federated_client_credentials`; no implicit default. |
| `issuer` | String | Required exact trusted JWT issuer; HTTPS URL with host, no userinfo/query/fragment. |
| `jwks_url` | String | Required HTTPS signing-key URL with host, no userinfo/query/fragment. |
| `audience` | String | Required nonempty incoming JWT audience; returned in credentials. |
| `identity_reference` | String | Optional non-secret attribution label. |
| `token_endpoint` | String | Federation only: HTTPS URL with fixed authority, optional declared path placeholders; no userinfo/query/fragment. |
| `scopes` | Array of strings | Federation requires a nonempty list; use nonempty provider-supported scopes. Other modes omit or use `[]`. |
| `parameters` | Object of regex strings | Federation only: placeholder names to full-match validation patterns. Names are identifiers, cannot be `client_id`; patterns must compile. Other modes omit or use `{}`. |

| Mode | Upstream Authorization behavior |
| --- | --- |
| `none` | Removes caller authorization. |
| `static` | Requires an explicit Authorization header (case-insensitive) in `headers`. |
| `passthrough` | Preserves validated caller JWT. |
| `federated_client_credentials` | Exchanges JWT as a client assertion using selected client ID, token endpoint and scopes; forwards exchanged token. |

Only `static` allows plan Authorization headers. Only federation permits token
endpoint/scopes/parameters. Substitutions may occur only in URL path segments,
never the scheme/hostname/port.

```json
[
  {
    "name":"federated-tools",
    "plan_id":"mcp-federated",
    "description":"App-bound tools access",
    "url":"https://tools.example/mcp",
    "auth": {
      "method":"federated_client_credentials",
      "issuer":"https://uaa.example/oauth/token",
      "jwks_url":"https://uaa.example/token_keys",
      "audience":"urn:tools:exchange",
      "identity_reference":"tools-identity",
      "token_endpoint":"https://identity.example/{tenant}/oauth/token",
      "scopes":["tools.read"],
      "parameters":{"tenant":"[a-z0-9-]+"}
    }
  }
]
```

OSB instance parameters for this example: `{"client_id":"tools-client","tenant":"team-a"}`.
`client_id` must be an identifier. Every declared parameter is required, nonempty,
a string and matched against the complete regex. Unknown parameters, `/`, `\`,
`?`, `#`, `%`, `{`, `}`, CR/LF and segments `.`/`..` are rejected. Other modes
accept no instance parameters. Backend/identity are snapshotted into gateway-owned
configuration at creation; operator edits affect new instances.

Bindings accept no identity parameters. They require `bind_resource.app_guid`,
`context.platform: "cloudfoundry"` and trusted
`context.service_account: {"issuer":"<configured issuer>","subject":"<exact CF account subject>"}`.
Assign the service account before binding. Plan service keys and updates are
unsupported. Credentials contain binding-specific `mcp_url`, `audience`, `issuer`,
`subject`, `auth_method`, `trusted_ca`, and no inference API key. Configure upstream
federation trust/permissions separately. Selected-identity capsules are attribution,
not proof of upstream authentication.

## Direct broker JSON configuration

`bin/broker -config FILE` reads the following fields. The CF launcher generates
this file; extra environment settings do not override generated values. Unknown
JSON fields are rejected. Arrays here are native JSON, not encoded JSON strings.

| Field | Type / requirement |
| --- | --- |
| `listen` | Required nonempty TCP address, e.g. `127.0.0.1:8080`. |
| `username`, `password` | Required nonempty Basic-auth strings. |
| `installation` | Required stable installation namespace. |
| `service_id` | Required nonempty OSB ID; launcher fixes `1b0b97a6-41df-4a39-9b23-53f17a5f85be`. |
| `gateway_url`, `management_url` | Required URLs with rules above. |
| `management_token` | Optional directly, but required with bundled token-protected gateway. |
| `mcp_url` | Required with nonempty `mcp_servers`; optional otherwise. |
| `inference_plans`, `mcp_servers` | JSON arrays with schemas above; at least one published plan. |
| `ca_file` | Optional PEM path added to system roots for broker-to-management TLS; not exposed by CF launcher. |
| `trusted_ca_file` | Optional PEM path returned to consumers; launcher writes it from `GATEWAY_TRUSTED_CA`. |
| `certificate`, `private_key` | Optional broker server TLS file paths; configure both or neither. CF normally uses Gorouter TLS termination. |

```json
{
  "listen":"127.0.0.1:8080",
  "username":"broker",
  "password":"REPLACE_WITH_BROKER_PASSWORD",
  "installation":"example-installation",
  "service_id":"1b0b97a6-41df-4a39-9b23-53f17a5f85be",
  "gateway_url":"https://gateway.apps.example/v1",
  "management_url":"https://management.example:9090",
  "management_token":"REPLACE_WITH_MANAGEMENT_TOKEN",
  "ca_file":"/etc/gateway/management-ca.pem",
  "inference_plans":[
    {"id":"inference-basic","name":"basic","description":"Shared inference allowance","free":true,"models":["chat-small"],"budget":10,"window":"24h"}
  ],
  "mcp_servers":[]
}
```
