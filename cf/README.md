# Deploying to Cloud Foundry

Deploy the broker and gateway as separate CF apps, with a PostgreSQL service bound
to the gateway. The supplied manifest starts two gateway instances and one broker.

## Prerequisites

- CF CLI access to your target organization and space, including permission to
  deploy apps, bind services, and create app-to-app network policies.
- Permission to register a service broker and enable access to its plans.
- A foundation with `cflinuxfs5`, the binary buildpack, container networking, and
  an internal domain such as `apps.internal`.
- A PostgreSQL service whose binding supplies a `postgres://` or `postgresql://`
  connection string in `uri` or `url`. The database role must be able to create
  and update the gateway's schema. Select persistence and backups to suit your needs.
- Upstream model-provider access, credentials, and pricing information.
- Docker and the [development tools](../README.md#development) for source builds.

## Build

Initialize the pinned gateway submodule, run `devbox run vendor`, then
`devbox run build`. Artifacts are written to `cf/dist/gateway` and `cf/dist/broker`.

The gateway builds in a digest-pinned Rust/Bookworm image without embedded UI
assets, retaining the management API. The current build profile uses one job,
disables LTO and optimization to reduce build memory. Generated binaries and
deployment secrets are ignored by Git.

## Configure

Target your organization and space, then create a PostgreSQL service instance
named `agentgateway-state` using an offering and plan available on your foundation:

```sh
cf target -o YOUR_ORG -s YOUR_SPACE
cf marketplace
cf create-service POSTGRES_OFFERING POSTGRES_PLAN agentgateway-state
```

Prepare an untracked vars file for `cf/manifest.yml` with these values:

| Variable | Value |
| --- | --- |
| `domain` | Your public CF application domain |
| `broker_password` | A strong password for the broker's `broker` Basic-auth user |
| `management_token` | A separate strong token shared by the broker and gateway |
| `gateway_models` | JSON string containing native agentgateway model definitions |
| `model_catalog` | JSON string containing the native model cost catalog |
| `trusted_ca` | PEM issuer CA bundle for the public inference route, or an empty string when consumers use system trust |

The sample manifest currently selects the alias `deepseek-v4-flash`, a USD allowance
of 10, and a `24h` window. Set its broker `MODEL_ALIASES`, `USD_ALLOWANCE`, and
`BUDGET_WINDOW` environment values for your plan. The model aliases must match
`gateway_models`; catalog rates must match your provider's reported usage.
See the pinned [agentgateway source](../src/agentgateway) for its configuration schema.

Keep the management token consistent across replicas, and use a distinct broker
`INSTALLATION` value for installations sharing a gateway. If your internal domain
differs, update the manifest's `MANAGEMENT_URL` and the discovery route below.

## Deploy and connect the apps

Push without starting so the PostgreSQL binding and network policy are configured
before the gateway initializes:

```sh
cf push -f cf/manifest.yml --vars-file /path/to/vars.yml --no-start
cf map-route agentgateway apps.internal --hostname agentgateway-management
cf add-network-policy agentgateway-broker agentgateway --protocol tcp --port 9090
```

Configure network access from the gateway to PostgreSQL and the selected providers.
Some service brokers install database app-to-app policy on bind; others require
operator-configured security groups or network rules. Do not assume binding alone
grants network access. The launcher reads the database URI from `VCAP_SERVICES`.

```sh
cf start agentgateway
cf start agentgateway-broker
```

Gorouter terminates public TLS and forwards inference to `$PORT`. Management runs
on private port 9090 with its own token. The internal route provides container
discovery; it does not expose management through the public inference route.
Raw admin/debug remains loopback-only.

## Register and consume the service

Register `https://agentgateway-broker.YOUR_DOMAIN` using the `broker` username and
configured password. The CLI can read the password from `CF_BROKER_PASSWORD` rather
than a command-line argument. Cloud Controller must trust the broker route's TLS
issuer and be able to reach it.

```sh
cf create-service-broker agentgateway broker https://agentgateway-broker.YOUR_DOMAIN
cf enable-service-access agentgateway
cf create-service agentgateway shared inference
cf bind-service YOUR_APP inference
```

Restart or restage the consumer to load the binding. App bindings and service keys
return `url`, `api_key`, and `trusted_ca`. Clients send the key as a Bearer token and
use the supplied PEM bundle for TLS verification. If the bundle is empty, use
system trust. Each service instance has one key shared by its bindings; unbind
does not revoke that key, while deprovision removes it.

## Verify

- Check gateway and broker instances with `cf apps`.
- Check broker -> gateway TCP/9090 policy and database/provider connectivity.
- Exercise provision, bind, service keys, unbind and delete through CAPI.
- Call `/v1/models` and `/v1/chat/completions`, including streaming, from a bound app.
- Scale the gateway and confirm new replicas recognize existing credentials.
- Restart gateway instances and confirm configuration and persisted usage survive.
- Confirm public tenant requests cannot access management or raw debug endpoints.

Budgets charge after responses and synchronize across replicas periodically.
Concurrent requests, streams and synchronization lag can overshoot an allowance.
Missing provider usage or catalog pricing can leave requests uncharged; verify
accounting with your chosen models before relying on it.

## Development checks

`devbox run dev` runs two local gateways, PostgreSQL and a
mock provider. It needs a compatible Linux runtime; on NixOS, set `GATEWAY_BINARY`
to a locally built compatible gateway or run the harness in a compatible container.

The scripts directory contains only development tasks: `build`, `vendor`, `test`,
and the Bash integration harness `dev`. Deployment uses the CF CLI procedure
above. Historical validation results and environment-specific recovery steps are
recorded in [lab validation](verification.md).
