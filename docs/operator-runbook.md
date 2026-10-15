# MVP operator runbook

## Development

The release pins agentgateway main at `c0354ea4`, with no UI assets. Its private
management API remains available. The broker is Go with standard-library-only
dependencies. Both compiler packages are vendored into this release.

1. `git submodule update --init --recursive`
2. `devbox install`
3. `devbox run -- rustup toolchain install 1.99.0 --profile minimal`
4. `devbox run -- bundle install`
5. `devbox run test`
6. `devbox run build`
7. `devbox run vendor`
8. `devbox run -- python scripts/spike.py`

Local Cargo dependency staging is ignored; regenerate before release creation.
The gateway package requires the vendored crates and source-replacement config.
Do not include source `target/` output in the BOSH package.

Local Go builds use `/dev/shm` for temporary files because this workstation's
temporary filesystem fails Go compiler mmap operations. This setting is local
development configuration, not a BOSH runtime requirement.

## Release and blobstore

`config/final.yml` points to a local `final_blobs` store. `.gitattributes` declares
LFS tracking; blob objects remain uncommitted until permanent blobstore setup.
Compiler vending produced `.final_builds` metadata and local final blob objects.
Preserve the metadata when moving the blobstore; rehydrate compiler blobs from the
pinned upstream releases recorded in `config/sources.yml` for a clean build.

Create a dev tarball with `bosh create-release --force --tarball /tmp/agentgateway-broker.tgz`.
Compilation requires native toolchain support for AWS-LC/allocators. A local Nix
build alone is not a successful Noble compilation/runtime check.

## Homelab topology

`manifests/homelab.yml` uses existing PostgreSQL 57.0.0 and BPM 1.4.41 releases,
Ubuntu Noble 1.562, and the default network. PostgreSQL is singleton and colocated
with the stateless broker, on a persistent disk. Gateways start with two replicas.
The initial PostgreSQL role is privileged for gateway schema initialization.

Supply these non-secret/operator inputs through a vars file:

- `model_aliases`: exact aliases allowed by the plan.
- `gateway_models`: native agentgateway model configuration, including upstream
  credentials; protect this vars file.
- `model_catalog`: native pricing catalog (`providers`/`models`/`rates`).
- `usd_allowance`: positive numeric USD allowance.
- `budget_window`: gateway duration, for example `24h`.

`manifests/dgx-spark-vars.yml` selects the OpenCode `atlas` provider's
`deepseek-v4-flash` at `http://192.168.2.148:8000/v1`, with no upstream API key.
Its $1/million input/output token rates are explicit internal allowance rates for
self-hosted inference. Confirm or adjust them before offering the plan.
Use `bosh -d agentgateway-broker deploy manifests/homelab.yml -l manifests/dgx-spark-vars.yml`.

CredHub generates broker/database/management credentials and TLS identities.
Apps and Cloud Controller must trust the generated CA and be able to resolve and
reach the BOSH DNS aliases. The management TLS port is 8444; restrict its network
access to broker/operator clients. Its key is distinct from tenant credentials.
The raw admin/debug listener binds only to loopback.

The gateway deployment includes its DNS jobs. Noble requires
`configure_systemd_resolved: true`, `override_nameserver: false`, and
`disable_recursors: true`. For this CF deployment, apply `manifests/cf-aliases-ops.yml`
to its existing alias addon rather than adding another colocated alias job.
The standalone DNS runtime example only applies to deployments without that job.
Verify aliases via `cf ssh opencode-demo` and `getent ahostsv4`.

Access homelab with `source ./bosh.env`. Upload the dev release, deploy with your
operator vars, then register the broker at `https://broker.agentgateway.internal:8443`
using its Basic-auth credentials. Enable service access to `agentgateway`.

`scripts/register-broker.py` automates registration/update without printing
credentials and installs the CA into the current CF control VM. This is a homelab
bootstrap step; persist CA trust through your CF deployment configuration before
recreating that VM. Bind `manifests/cf-egress.json` as a running ASG to the app's
space; it allows private gateway inference on port 8443. Allow dynamic ASG
propagation time or restart the app.

The homelab broker is registered, service `agentgateway-opencode` is bound to
`opencode-demo` in the recreated `poc/demo` space. Run
`python scripts/app-acceptance.py` to test with its VCAP_SERVICES credentials and
verified TLS inside the Diego container. The acceptance client consumes
`trusted_ca` directly from the binding, without external CA files or environment
workarounds. Configure the OpenCode provider to consume that same field.

Refresh existing bindings using `cf unbind-service` followed by `cf bind-service`;
the shared instance key survives unbind. `scripts/refresh-binding.py` verifies key
preservation and CA validity without printing credentials. Coordinate app restart
with its owner: rebind does not update a running process's VCAP_SERVICES.

## CF semantics

- One random API key per service instance, stored in gateway resources.
- All app bindings and service keys receive `{url, api_key, trusted_ca}` for that key.
  `trusted_ca` supplies the inference CA PEM; clients must use it for TLS verification.
- Provision retries find metadata labels and compare request fingerprints.
- Relies on CAPI serializing same-instance lifecycle operations; no detached worker.
- Unbind leaves the shared key valid; deprovision deletes it.
- No plan updates, custom parameters, async operations, or advertised retrieval.
- Budgets settle after responses. Replica synchronization, concurrent requests,
  and streams can overshoot. Abrupt crashes can lose unflushed usage.
- Provider usage and correct catalog pricing are required for USD charges.

## Acceptance

Run CF create-service, bind/restage, models/chat/SSE, service-key, unbind and delete.
Confirm shared credentials/budget, independent instances, revocation propagation,
restart recovery, scale-out, and graceful rolling updates. Also exercise lost API
responses and delayed write completion. Do not count local fixtures as proof of
CF container networking or BOSH compilation.
