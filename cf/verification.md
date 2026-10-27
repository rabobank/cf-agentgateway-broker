# CF migration verification — 2026-10-26

This is a historical record of one local lab validation, not a deployment guide
or a set of requirements for other foundations. The ephemeral plan, accepted state
loss, migration from BOSH, addresses and consumer names apply only to that lab.
For deployment instructions, see [Deploying to Cloud Foundry](README.md).

## Running topology

- `poc/demo`: agentgateway 2/2, broker 1/1, both binary buildpack on cflinuxfs5.
- PostgreSQL/ephemeral service `agentgateway-state`, supplied by cf-esb.
- PostgreSQL app runs in `poc/cf-esb-instances`.
- Public inference: `https://agentgateway.10.246.0.21.sslip.io/v1`.
- OSB: `https://agentgateway-broker.10.246.0.21.sslip.io`.
- Private management DNS `agentgateway-management.apps.internal`, port 9090,
  with broker-only app-to-app policy and distinct management token.
- Database binding installs gateway -> PostgreSQL TCP/5432 policy.

## Passed

- Gateway built from pinned source with vendored dependencies; no embedded UI.
- Standard glibc binary runs in cflinuxfs5, without Nix runtime dependencies.
- Go broker/launcher tests, race detector, vet and existing template tests.
- Broker private management readiness returns ready.
- Real OSB provision, bindings, service-key creation/deletion and deprovision.
- Recreated agentgateway-opencode and agentgateway-rabo-os services/bindings.
- opencode-demo and rabo-os-demo restarted and are 1/1.
- Actual OpenCode app container models call returns 200 with binding-provided
  issuer CA trust; no external trust workaround required.
- Gateway restart retained existing keys while PostgreSQL stayed running.
- Scaled 2 -> 3: all three containers accepted the same existing key locally.
  Returned to 2/2 after verification.
- Tenant credentials receive 403 on public `/api/config` and `/config_dump`.
- Old BOSH agentgateway deployment was already absent before migration.

## Fixes encountered

- cf-esb needed CF API egress and current managed-instance space GUID after the
  foundation reset. Its stale GUID was corrected.
- PostgreSQL binding duplicated `.apps.internal`; the cf-esb session fixed and
  deployed its hostname normalization/portless discovery route. Rebinding resolved it.
- Supplying a route leaf certificate did not establish client trust in Ruby.
  Retrieved the router certificate's issuer CA from platform CredHub and put that
  public PEM into broker configuration. Bindings were refreshed after correcting it.
- Optimized builds exceeded this VM's memory. The MVP artifact uses single-job
  unoptimized release profile without LTO; an optimized CI build is a later improvement.

## Real-provider acceptance

Spark was temporarily offline while another session evaluated Atlas. After that
session restored DeepSeek, the in-container acceptance check passed from opencode-demo:
models, non-streaming chat and SSE all returned 200 over verified TLS using only
the binding's issuer CA. Response content was `gateway works`.

The ephemeral PostgreSQL database persisted USD budget usage of 139000 nanodollars
after the acceptance calls. This confirms priced usage flows into shared state.

State is deliberately disposable. PostgreSQL replacement loses all keys/config
and usage, requiring consumer service recreation. No old-state migration occurred.
