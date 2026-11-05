# cf-agentgateway-broker

A stateless Go Open Service Broker API adapter and source-built,
headless [agentgateway](https://agentgateway.dev/) deployed as Cloud Foundry apps.

## Deployment

- `agentgateway`: horizontally scalable inference app, initially two instances.
- `agentgateway-broker`: stateless OSB app using the private gateway API.
- `agentgateway-state`: PostgreSQL service instance for configuration and usage.
- `capsule-extmcp`: Go sidecar per gateway for shared MCP authentication and
  service-instance-aware capsules with a PostgreSQL-backed checkpointed log.

CF routes terminate TLS and distribute inference connections. Private container
network policies restrict access to gateway management. App bindings and service keys receive `url`, `api_key`, and PEM
`trusted_ca`. All bindings for an instance share its key and best-effort USD budget.
Unbind preserves the key; deprovision revokes it.

## Development

1. `git submodule update --init --recursive`
2. `devbox install`
3. `devbox run vendor`
4. `devbox run test`
5. `devbox run build` (requires Docker)

The source, Cargo package used for vendoring, and Docker build image are pinned.
Cargo dependencies are vendored locally; build artifacts and deployment secrets
are ignored.

Run `devbox run dev` after building to exercise two local gateways, PostgreSQL,
and a mock provider without paid inference calls. It checks streaming, shared
budgets, restart recovery, and broker lifecycle operations, then stops the local
processes and prints the retained log directory. Override `DEV_PORT_BASE` if the
default port range conflicts with another service. On NixOS, use `GATEWAY_BINARY`
to select a locally built compatible gateway binary.

Run `devbox run acceptance` after building for the Ginkgo acceptance suite. It
starts a temporary PostgreSQL cluster using Devbox, hosts Higress's mock LLM
provider in the test process, and runs the built gateway and broker in
`cflinuxfs5` containers using Linux Docker host networking. Port 9090 must be
free for the gateway management API. The suite cleans up its containers and
database on exit. Docker and the initialized Higress Git submodule are required.

The weekly **Bump agentgateway** workflow checks the latest stable upstream
release. It opens a pull request only after the candidate passes the acceptance
workflow. Repository or organization Actions settings must permit GitHub Actions
to create pull requests. The acceptance result is linked in the pull request;
GitHub may require approval before running additional PR-triggered workflows.

To publish a release, run the **Release** GitHub Actions workflow on the default
branch with a new stable version such as `v1.2.3`. It runs acceptance tests before
creating the release, then attaches a single tested Linux AMD64 deployment
tarball and its SHA-256 checksum. Unpack it and follow its README to configure
the included manifest and deploy with `cf push`. The checked-in `cf/dev-vars.yml`
contains the local lab's non-secret settings; `cf/local-secrets.yml` holds the
lab's private values and is ignored by Git.

See [deployment instructions](cf/README.md), [lab validation results](cf/verification.md),
[architecture](docs/architecture.md), and [source findings](docs/source-investigation.md).
Shared operator-configured MCP servers and capsule recording are described in
[MCP and capsules](docs/mcp-capsules.md).

Choose a PostgreSQL service with persistence and backup capabilities appropriate
for your deployment. The validation environment uses an ephemeral development plan;
that is not a requirement of the broker.
