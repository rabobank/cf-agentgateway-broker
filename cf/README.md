# Cloud Foundry deployment

Target: two CF apps and an ephemeral PostgreSQL service instance. State is
disposable; consumers and service instances are recreated when switching from
the old BOSH broker. The broker never connects to PostgreSQL directly.

## Build

1. Initialize the pinned gateway submodule and run `devbox run vendor`.
2. Run `devbox run -- bash scripts/build-cf`.

The gateway compiles in a digest-pinned Rust/Bookworm image, retaining the
management API without UI assets. The memory-constrained MVP profile disables LTO
and optimization and uses one build job. Deploy to cflinuxfs5. Artifacts and local secrets are
ignored; no binaries or LFS blob objects should be committed.

## Homelab prerequisites

- Target `poc/demo` explicitly before provisioning/deploying.
- `cf create-service PostgreSQL ephemeral agentgateway-state`
- The cf-esb broker must have CF API egress and its managed-instance space GUID
  must refer to the current foundation's space. If the foundation was recreated,
  refresh `CF_INSTANCE_SPACE_GUID` before provisioning.
- `python scripts/configure-cf.py --trusted-ca /path/to/route-ca.pem` creates ignored
  `cf/local-vars.yml` with fresh secrets and the supplied inference route issuer
  CA bundle. It verifies the route with that trust before writing vars. Obtain
  the issuer CA from platform configuration/CredHub, not the route's leaf cert.
- Bind the DGX Spark egress ASG to the gateway's space.

## Deploy

`python scripts/deploy-cf.py` pushes both binary-buildpack apps, maps a portless
`agentgateway-management.apps.internal` discovery route, grants only the broker
TCP/9090 access through CF network policy, and starts the apps. Public inference
uses `$PORT` behind Gorouter TLS. Raw admin/debug binds to loopback. Management
requires a separate operator token and is not attached to the public route.

The PostgreSQL service binding installs app-to-app TCP policy for the gateway.
It supplies a `uri` through VCAP_SERVICES. Gateway configuration is generated at
startup with that URI and environment-provided models/catalog. Both gateway
replicas share that database. Ephemeral PostgreSQL loses state on replacement.

The script updates the existing `agentgateway` broker registration. Recreate old
service instances and bindings explicitly; they cannot be recovered from the new
empty database. Credential schema remains `url`, `api_key`, `trusted_ca` (PEM).
Refresh app environments by restarting/restaging after coordinating consumers.

## Verification

- `cf apps`: gateway 2/2 and broker 1/1.
- `cf network-policies`: broker -> gateway TCP/9090 and gateway -> PostgreSQL/5432.
- OSB provision/bind/service-key/unbind/delete through CAPI.
- Models, chat and SSE from the actual OpenCode CF container with binding trust.
- Scale gateway to three instances and verify new replicas load existing keys.
- Restart replicas and verify credentials/usage survive while PostgreSQL remains up.
- Public inference must not expose `/api/config` or raw admin/debug endpoints.

The operator allowance catalog uses $1/million input and output tokens for
self-hosted DeepSeek, not a provider billing quote. Replica accounting retains
the existing post-response/eventual semantics.
