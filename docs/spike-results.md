# Initial implementation verification

Date: 2026-10-13. Source: main `c0354ea426b25b4b3df357ba4b6afabcf4309660`.

## Passed locally

- Headless source build with Rust 1.99.0, default crypto/allocator features and
  no `ui/dist`. Management API available without Node/UI compilation.
- Go lifecycle tests with race detector and `go vet`.
- BOSH template rendering tests: secret JSON serialization, encoded database
  credentials, separate management/inference policy, PostgreSQL link rendering.
- Homelab deployment interpolation with generated test credentials/certificates.
- Cargo frozen metadata resolution with vendored registry and Git dependencies.
- Dev release `0+dev.1` created and uploaded to the homelab director.
- BOSH dry-run template/link validation passed after supplying DNS API TLS variables.
- `scripts/spike.py`: two independent real gateway processes sharing PostgreSQL 18
  and a deterministic mock OpenAI provider. Verified management credential required,
  raw key/metadata round trip, key activation on both replicas, non-streaming and
  SSE completion, shared USD exhaustion on both replicas, persisted usage after
  replica restart, and deletion/revocation on both replicas. Fixture provider
  checks its upstream credential and rejects tenant-key forwarding.
- Real Go broker against those gateways: provision/idempotent retry, app binding
  and service key share credentials, unbind preserves access, deprovision revokes
  both replicas.

The spike polls for convergence and does not establish a strict convergence bound.
It is not a fault-injection/exactly-once accounting proof. Some polling requests
can add usage before exhaustion propagates, consistent with accepted overshoot.

## Remaining deployment gates

Initial deploy task 33419 was interrupted by a homelab restart and marked timeout.
Restarted as task 33458 using DeepSeek/DGX Spark vars. Both compiler packages had
compiled on Noble; frozen gateway compilation resumed on a fresh compilation VM.
Spark connectivity is being investigated by a separate session.

## Homelab results

- Frozen source compilation on Ubuntu Noble succeeded (gateway task 33458).
- Fixed Go packaging to set GOCACHE explicitly in the no-HOME compilation environment.
- Fixed BPM Monit commands to specify their named processes.
- Fixed Noble BOSH DNS integration with systemd-resolved.
- Release `0+dev.5` deployed successfully, task 33537. Broker, PostgreSQL and two
  gateway replicas run on three BOSH VMs. Gateway libgcc runtime is packaged.
- Recovered CF from the homelab outages and applied DNS aliases to its existing
  addon (task 33502). No second alias job/runtime config remains active.
- Target CAPI advertises OSB 2.15; broker accepts compatible 2.x from 2.15 onward.
- Broker registered; real service provision and app binding succeeded.
- `opencode-demo` resolves both gateway replica addresses through BOSH DNS.
- Space-scoped inference ASG created/bound to `poc/demo` for port 8443.
- Verified-TLS models, chat and SSE from `opencode-demo` on Diego to DeepSeek 4
  at DGX Spark succeeded. Response content was `gateway works`.
- PostgreSQL budget_usage held 159000 nanodollars ($0.000159) after the initial
  two chat calls, proving nonzero persisted USD accounting with operator rates.
- App binding and real service key returned identical shared credentials.
- Temporary service/key create/delete lifecycle succeeded; demo binding retained.

The acceptance client runs within the actual OpenCode app container and consumes
VCAP_SERVICES. Scale-out and fault-injection checks remain separate gates.

## Binding-provided TLS trust

- Broker release `0+dev.6` deployed successfully (task 33556).
- Binding and service-key credentials include `trusted_ca`, the inference CA PEM
  bundle. Management CA configuration is separate from inference trust inputs.
- Rebound `agentgateway-opencode` to `opencode-demo` in recreated `poc/demo`
  (space `951bb3f5-4c9c-4be6-ab4c-fd3fbfba34b3`). Verified the instance API key and
  URL were preserved, and the returned CA was parseable, currently valid and
  usable as TLS trust. No app restart was performed by the broker session.
- A temporary real service key returned identical credentials including trusted_ca;
  the verification key was deleted after checking.
- The buildpack session removed manual CA environment/file workarounds, restaged
  the app with binding-only trust, and verified real OpenCode chat returned
  `gateway works` without TLS errors. Its only provider/model is
  `agentgateway/deepseek-v4-flash`.
- Demo setup now binds the inference ASG when recreating the demo space.
- `scripts/app-acceptance.py` consumes CA certificates directly from binding
  credentials; `scripts/refresh-binding.py` refreshes/verifies bindings without
  printing credentials or restarting the app.

- Noble offline compilation and runtime-library closure. The local Nix executable
  links Nix glibc/libgcc and is not used as the BOSH deployment binary.
- CF-container DNS resolution, CA trust, gateway reachability and CAPI broker access.
- Actual CF create/bind/restage/inference/service-key/unbind/delete.
- Scale-out, PostgreSQL listener reconnect, graceful rolling updates and delayed
  remote-write/lost-response retry testing.
- Pricing/usage validation for the real operator-selected model/provider.

Build/spike logs are ignored under `test/.work/`. No blob objects were committed.
