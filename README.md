# agentgateway-broker-release

## CF deployment

The active deployment uses two CF apps plus an ephemeral PostgreSQL
service instance. See [CF deployment](cf/README.md) for source builds, private
management networking, deployment and consumer recreation. Existing BOSH release
files remain available as the historical deployment option.

A stateless Go Open Service Broker API adapter and
source-built [agentgateway](https://agentgateway.dev/) from pinned main.
Cloud Foundry bindings receive native API-key credentials; bindings to one service
instance share a model allowance and best-effort USD spend budget.

## MVP design

- Gateway API resources are the broker's source of truth; no broker database.
- Random shared key per instance, recovered through private gateway management.
- Horizontally scalable gateways with PostgreSQL-backed configuration and usage.
- CF routes distribute inference; private container networking connects management.
- PostgreSQL is an ephemeral CF service; only the gateway binds to it.
- Headless gateway build retaining management APIs.
- Unbind preserves the shared key; deprovision revokes it.

Start with the [operator runbook](docs/operator-runbook.md) and
[source investigation](docs/source-investigation.md).

Run `devbox run test`, `devbox run build`, and `devbox run vendor`.
`devbox run -- python scripts/spike.py` exercises two real gateways against local
PostgreSQL and a mock provider. See `manifests/homelab.yml` for deployment.
