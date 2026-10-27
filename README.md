# cf-agentgateway-broker

A stateless Go Open Service Broker API adapter and source-built,
headless [agentgateway](https://agentgateway.dev/) deployed as Cloud Foundry apps.

## Deployment

- `agentgateway`: horizontally scalable inference app, initially two instances.
- `agentgateway-broker`: stateless OSB app using the private gateway API.
- `agentgateway-state`: PostgreSQL service instance for configuration and usage.

CF routes terminate TLS and distribute inference connections. Private container
network policies restrict access to gateway management. App bindings and service keys receive `url`, `api_key`, and PEM
`trusted_ca`. All bindings for an instance share its key and best-effort USD budget.
Unbind preserves the key; deprovision revokes it.

## Development

1. `git submodule update --init --recursive`
2. `devbox install`
3. `devbox run -- rustup toolchain install 1.99.0 --profile minimal`
4. `devbox run vendor`
5. `devbox run test`
6. `devbox run build` (requires Docker)

The source and Docker build image are pinned. Cargo dependencies are vendored
locally; build artifacts and deployment secrets are ignored.

Run `devbox run dev` after building to exercise two local gateways, PostgreSQL,
and a mock provider without paid inference calls. It checks streaming, shared
budgets, restart recovery, and broker lifecycle operations, then stops the local
processes and prints the retained log directory. Override `DEV_PORT_BASE` if the
default port range conflicts with another service. On NixOS, use `GATEWAY_BINARY`
to select a locally built compatible gateway binary.

See [deployment instructions](cf/README.md), [lab validation results](cf/verification.md),
[architecture](docs/architecture.md), and [source findings](docs/source-investigation.md).

Choose a PostgreSQL service with persistence and backup capabilities appropriate
for your deployment. The validation environment uses an ephemeral development plan;
that is not a requirement of the broker.
