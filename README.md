# agentgateway-broker-release

A BOSH release containing a stateless Go Open Service Broker API adapter and
source-built [agentgateway](https://agentgateway.dev/) from pinned main.
Cloud Foundry bindings receive native API-key credentials; bindings to one service
instance share a model allowance and best-effort USD spend budget.

## MVP design

- Gateway API resources are the broker's source of truth; no broker database.
- Random shared key per instance, recovered through private gateway management.
- Horizontally scalable gateways with PostgreSQL-backed configuration and usage.
- BOSH DNS distributes connections; no load balancer.
- Singleton PostgreSQL colocated with the broker; BPM manages app processes.
- Headless gateway build retaining management APIs.
- Unbind preserves the shared key; deprovision revokes it.

Start with the [operator runbook](docs/operator-runbook.md) and
[source investigation](docs/source-investigation.md).

Run `devbox run test`, `devbox run build`, and `devbox run vendor`.
`devbox run -- python scripts/spike.py` exercises two real gateways against local
PostgreSQL and a mock provider. See `manifests/homelab.yml` for deployment.
