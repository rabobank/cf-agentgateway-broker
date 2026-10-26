# agentgateway CF broker

A stateless Go Open Service Broker API adapter and source-built,
headless [agentgateway](https://agentgateway.dev/) deployed as Cloud Foundry apps.

## Deployment

- `agentgateway`: horizontally scalable inference app, initially two instances.
- `agentgateway-broker`: stateless OSB app using the private gateway API.
- `agentgateway-state`: PostgreSQL / ephemeral service instance from CF-ESB.

CF routes terminate TLS and distribute inference connections. Private container
network policies connect the broker to gateway management and the gateway to
PostgreSQL. App bindings and service keys receive `url`, `api_key`, and PEM
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

See [deployment instructions](cf/README.md), [verification results](cf/verification.md),
[architecture](docs/architecture.md), and [source findings](docs/source-investigation.md).

PostgreSQL is deliberately ephemeral: replacement loses keys, configuration and
usage. Consumers must recreate their service instances and bindings afterward.
