# Operator configuration and multi-offering refactoring

Status: implementation plan. The configuration examples and extension points in
this document describe the target design, not functionality accepted by the
current binaries. No production deployment is part of this plan's execution.

## 1. Goals and agreed decisions

1. Publish multiple LLM offerings and multiple MCP offerings, each with multiple
   plans. An offering contains one protocol; inference and MCP are not combined
   in the same service.
2. Maintain two application configuration files: `broker.yml` and `gateway.yml`.
   Use ordinary YAML objects, arrays, numeric values and PEM block scalars rather
   than JSON embedded inside YAML strings.
3. Keep `cf/manifest.yml` and CF manifest interpolation. Deployment properties
   such as routes, app names, memory, replicas and service bindings remain there
   and in deployment vars files.
4. Convert application YAML to JSON strings in dynamically generated vars files.
   Set one operator configuration environment variable, `AGENTGATEWAY_CONFIG`, on
   each app, with different contents for the broker and gateway.
5. Expose operator decisions, not internal gateway wiring. Pass selected native
   agentgateway configuration blocks through without maintaining a parallel
   provider or authentication schema.
6. Configure consumer connection URLs per offering, not globally and not per
   provisioned instance. MCP binding credentials can derive a binding-specific
   path from the offering's public MCP base URL.
7. Support shared LLM providers referenced by several models. Plans grant access
   to named models or MCP backends.
8. Select upstream MCP identity at **instance creation**, not binding time. For
   the Azure DevOps federation example, the developer supplies the application
   client ID. All bindings of that instance use that selected upstream identity.
9. Allow each MCP target to use its own native upstream authentication policy.
   Caller authentication and binding authorization remain separate requirements.
10. Keep the broker a stateless OSB-to-management-API translator. Gateway-owned
    resources remain authoritative for instance selections, grants and lifecycle
    metadata. No broker database connection, tables, migrations or grant store.

Production currently has no provisioned instances. This permits a coordinated
catalog/configuration replacement rather than an automatic migration of existing
instances. Preserve installation and cryptographic keys where already deployed.

## 2. Current implementation and change surface

| Area | Current behavior | Required change |
| --- | --- | --- |
| `src/agentgateway-broker/main.go` | One service ID, hard-coded service name, flat inference plans | Offering-aware catalog, configuration and lifecycle resolution |
| `src/agentgateway-broker/mcp.go` | Backend selected by its plan ID | Resolve offering/plan, then named backend access and stored selections |
| `src/mcpconfig/config.go` | Custom backend/auth schema; backend snapshots embedded in instances and grants | Shared resource references, explicit parameter contracts, generation-safe effective policy |
| `src/cf-launcher/main.go` | Many environment variables; synthesizes model-only LLM config | Consolidated JSON input, native providers/models, opinionated CF assembly |
| `src/cf-launcher/mcp.go` | Translates custom MCP auth/backend definitions | Native backend blocks plus generated CF enforcement |
| `src/capsule-extmcp/main.go` | Separate operator environment variables | Read its settings from gateway app configuration |
| `cf/manifest.yml` | Many application placeholders | Two JSON-string placeholders, one per app |
| `cf/deploy` | Positional deployment arguments and management URL env lookup | Retain deployment interface; read management URL from consolidated config |
| `scripts/build`, `.github/workflows/release.yml` | Current bundle contents | Package examples, conversion utility and revised documentation |
| `acceptance/`, development scripts and workflows | Existing configuration contract | Migrate fixtures and invocations alongside runtime changes |

The gateway already has native named LLM providers/model references and named
backends. These do not by themselves prove that per-instance federation,
per-binding sessions or generated-route policy inheritance work with shared MCP
references. Establish those contracts before changing lifecycle generation.

## 3. Configuration contract

### 3.1 Broker file

The broker owns the OSB configuration schema:

- `installation`: stable gateway resource namespace.
- `auth`: OSB username/password.
- `gateway.management_url`: management API base URL.
- Optional management trust customization; system and CF-mounted trust suffice
  by default. Keep management trust distinct from consumer trust.
- `services[]`: offering `id`, `name`, `description`, `protocol`, `connection`
  and `plans`.
- `connection.url`: public gateway URL for that offering, with an optional
  consumer CA bundle when system trust is insufficient.
- Each plan: `id`, `name`, `description`, `free`, `access` and optional schemas.
- LLM access: explicit model aliases, positive USD allowance and budget window.
- MCP access: named backend references. Support a list rather than hard-coding
  one backend per plan; validate aggregated target naming and authorization.
- `schemas.service_instance.create.parameters`: standard OSB parameter schema,
  enforced by the broker as well as advertised in the catalog.

Do not advertise plan updates, binding retrieval or instance retrieval unless
implemented. Retain the current inference shared-key/unbind behavior and document
it accurately; this refactoring does not introduce inference binding-level keys.

Validation rules:

- Stable, nonempty service and plan IDs; service IDs unique and plan IDs globally
  unique. Service names unique; plan names unique within an offering.
- Nonblank descriptions, nonempty offerings/plans, supported protocol values.
- Reject mixed-protocol access, unknown broker fields and invalid URLs.
- Reject wildcard/empty model aliases and malformed/nonfinite allowances.
- Check that referenced resources exist and match the expected protocol.
- Reject unused, missing or conflicting instance parameter references.
- Retain exact service/plan ownership checks on every lifecycle operation.

### 3.2 Gateway file

Expose these operator-facing sections:

| Section | Purpose and ownership |
| --- | --- |
| `database.service_name` | Exact PostgreSQL binding selection; connection comes from `VCAP_SERVICES` |
| `llm.providers` | Native provider definitions, credentials, endpoint and provider-specific options |
| `llm.models` | Native model definitions and provider references |
| `model_catalog` | Native catalog data; assemble into the supported `config.modelCatalog` source |
| `mcp.backends` | Native named backend/target definitions and upstream policies |
| `mcp.caller_auth` | Operator-owned issuer/JWKS/audience settings using native JWT field shapes |
| `mcp.session_key` | Stable, replica-shared cryptographic key |
| `capsules` | Producer log identity, signing seed and optional witness settings |

Initially expose one caller trust policy for MCP routes. Multiple trust domains
would require an explicit policy-selection design; do not silently make trust
depend on the upstream token authority or individual request data.

Keep the native blocks as raw structured data where practical. Validate the CF
envelope and resource references without reimplementing Azure, OpenAI or native
backend policy schemas. Have the pinned gateway validate native blocks. Confirm
an available native validation path, or add one, before promising pre-deploy
validation of all native fields.

The integration supplies:

- Public listener port from `PORT`; internal listener names and management port.
- Gateway database URL from its service binding and the required storage mode.
- Management route isolation, CF client identity and trust handling.
- Strict inference API-key enforcement and supported API routes.
- MCP caller validation requirements and preservation of assertions where needed.
- Binding-specific subject checks, live grant checks and generation checks.
- Capsule/authorization sidecar addresses and request/metadata expressions.
- MCP session isolation and binding-specific routes.

Native per-model/target policies must not accidentally bypass generated frontend
authorization. Define precedence and reject conflicting uses of reserved
integration fields. Do not introduce a blanket arbitrary configuration override.

### 3.3 Schema ownership and evolution

Document the CF envelope versioning policy before shipping examples. Prefer an
explicit version if incompatible envelope evolution is expected; it must not be
forwarded as an unknown field into native gateway configuration.

Do not use a shared model type merely because both files use YAML. The broker
catalog and gateway provider definitions have different owners. Share only the
small envelope/reference contracts necessary for validation and integration.

## 4. Instance-time MCP identity

### 4.1 Parameter declaration

For a federated plan, advertise and enforce an OSB creation schema with required
`client_id`, string type, and an operator-appropriate identifier pattern. Reject
undeclared parameters. Choose and document the supported JSON Schema dialect and
keywords; do not advertise constraints the implementation does not enforce.

The application client ID selects the upstream principal. It is not a client
secret, object ID, access token or proof that federation/resource access works.
External principal creation, federation trust and resource permission assignment
remain prerequisites outside the broker.

### 4.2 Applying selections to native policy

An OSB schema validates input but does not connect it to `clientAuth.clientId`.
Implement a narrow, documented extension such as
`clientId: {fromInstanceParameter: client_id}`. Treat this as a CF integration
marker, not native gateway syntax.

Before finalizing the syntax:

1. Inventory required dynamic fields, starting with the federation client ID.
2. Allow references only at approved typed positions. Do not implement arbitrary
   YAML templating, CEL interpolation or string replacement throughout policies.
3. Validate references against every plan that exposes the target.
4. For multiple targets, permit distinct names such as `devops_client_id` and
   `tracker_client_id`; never infer that all targets use the same principal.
5. Keep authorities, endpoints, scopes and headers operator-controlled. Additional
   selectable identity fields require explicit constraints and typed handling.
6. Persist validated selections in the gateway-managed instance. Reject binding
   parameters that attempt to change the selected upstream identity.

Separate the stable shared backend definition from effective instance policy and
binding-specific session identity. Determine whether the gateway can resolve a
reference with trusted identity overrides safely. If not, extend its managed
resource model and normalization path rather than storing duplicated authoritative
backend/grant data in the broker.

### 4.3 Caller trust and Azure DevOps evidence

Reference: <https://gist.github.com/rkoster/211c8f74ae8e4ae37e83de3be6a0bfd7>.

The gist reports successful app-only WIF initialization and a repository listing
by a colleague. It does not establish the exact scope/FIC configuration, whether
`X-MCP-Insiders` is required, or successful integration with the intended UAA.

For acceptance:

- The incoming assertion issuer/JWKS/audience must match the actual CF workload
  token. Do not default these to Entra because Entra is the upstream authority.
- Entra's federated identity credential must match assertion issuer, subject and
  audience. Bind trusted CF context to the same caller identity.
- Verify the hosted MCP resource scope instead of assuming REST API scopes or
  treating prior sample values as confirmed.
- Prove initialize plus a permitted tool call, not token acquisition alone.
- Keep provider IDs, tenant IDs, resource scopes and endpoints in operator
  configuration. No Azure-specific selection logic in the broker.

## 5. Shared resources, updates and durable state

### 5.1 Ownership

Operator files define catalog and available backend/auth configuration. Gateway
configuration and managed lifecycle resources are authoritative at runtime.

An instance records installation, OSB IDs, provision fingerprint, selected access,
validated identity parameters, relevant contract revision and generation.
A binding records its caller grant, parent identity and generation. Shared secrets
do not belong in either record.

The broker accesses all durable state through management APIs. The sidecar may
continue reading gateway-fabric state for authorization; it must not become a
second authoritative catalog or grant store.

### 5.2 Explicit update semantics

- Plan permission/budget edits affect newly provisioned instances. Existing
  instance entitlements do not silently expand.
- Shared provider credential rotation updates provider configuration; do not
  regenerate instance API keys or budgets.
- MCP credential rotation should update the shared backend credential without
  rewriting every grant. Endpoint, auth mode, target membership and scope changes
  can alter authority, so they are not automatically equivalent to rotation.
- Establish a resource revision/compatibility mechanism for MCP updates. Freeze
  authorized target membership and identity semantics, or require an explicit
  migration when incompatible changes occur. A reference name alone is not enough.
- Define missing/deleted reference behavior as fail-closed. Prevent removal of
  referenced definitions or provide actionable diagnostics and a migration path.
- Specify offering URL/trust behavior: use current offering connection settings
  when returning bindings, preserving credentials and identity. Existing delivered
  credentials are not automatically updated; document rebind/route overlap needs.
- Keep random binding/instance generations and session/cache scoping distinct
  from stable resource IDs. Recreating a binding must not reuse old sessions.

Broker-local caches are disposable. Recovery and revocation cannot rely on them.
Retain gateway tombstone behavior against delayed writes. Verify uncertain outcomes
and orphan cleanup even when CAPI switches to a new binding GUID after failure.

## 6. Deployment and runtime plumbing

1. Change manifest application mappings to `AGENTGATEWAY_CONFIG:
   ((broker_config))` and `AGENTGATEWAY_CONFIG: ((gateway_config))` respectively.
   Keep platform variables and fixed process settings such as the gateway role
   and required runtime stack sizing separate from operator application settings.
2. Supply a small YAML-to-vars converter supporting invocations equivalent to
   `--vars-file <(converter broker_config broker.yml)` and the corresponding
   gateway command. Its output is a vars object whose value is a JSON string,
   not a nested CF environment object.
3. Pin/document the YAML implementation. Reject duplicate keys, multiple
   documents, non-string mapping keys, non-JSON values and nonfinite numbers.
   Preserve budgets, decimal pricing strings, PEM content and explicit empties.
4. Process-substitution failures are not reliably propagated by the outer CF
   command. Validate both files in a checked step before push; conversion must
   fail closed, never substitute an empty/default configuration.
5. Add cross-file validation for model/backend references without publishing the
   gateway's secrets into broker configuration. The broker needs a supported
   management-API view of backend parameter contracts/revisions at runtime;
   define and validate this gateway-owned metadata as part of the resource work.
6. Update `cf/deploy` to inspect consolidated broker configuration for management
   URL checks. Retain route destination validation and broker-only route policies.
7. Have the launcher decode the app-specific configuration, assemble native
   runtime files with restrictive permissions, and execute the intended binary.
   Never write configuration contents into diagnostics.
8. Have the capsule sidecar read its subsection itself: it is a separate CF
   process and cannot inherit environment mutations made by the gateway launcher.
9. Preserve CF identity reload and management TLS behavior. The broker remains
   unbound from PostgreSQL; gateway/sidecar select the same gateway-fabric binding.
10. Explicitly remove only the known retired operator variables on migration.
    Reject ambiguous simultaneous legacy/new configuration rather than silently
    choosing a precedence. Do not remove platform variables or unrelated settings.

## 7. Implementation phases

### Phase A — establish gateway contracts

- Inspect the revision in `scripts/agentgateway-version` and native fixtures for
  providers, backends, JWT policies, token exchange and catalog sources.
- Verify policy attachment/precedence on broker-generated routes; top-level
  `mcp.policies` is not automatically inherited by unrelated routes.
- Define shared backend references, parameter contract metadata, revision rules,
  effective per-instance policy and session/cache isolation.
- Inspect management API visibility and update semantics, including replica
  configuration propagation and static/native resource availability.
- Implement missing gateway primitives and contract tests first; update the pin
  through the repository's existing source/build workflow when necessary.

Exit: a tested gateway mechanism supports shared configuration and durable
instance identity without copying secrets into grants or weakening revocation.

### Phase B — configuration envelope and CF integration

- Implement consolidated configuration readers and the converter/validator.
- Assemble release-owned gateway wiring around native operator blocks.
- Integrate sidecar settings and manifest/deploy updates.
- Add complete examples with placeholders, including one provider/multiple models
  and several MCP auth modes. Include a federated instance parameter example.

Exit: a configuration round trip produces the intended runtime settings and native
validation succeeds; management connectivity works with CF identity/trust.

### Phase C — multi-offering broker

- Implement typed offering/plan configuration, validation and catalog generation.
- Resolve requests by service/plan pair; persist and verify ownership.
- Return per-offering connection settings and binding-specific MCP paths.
- Advertise/enforce instance parameter schemas and store resolved selections.
- Preserve existing retry, cleanup and lifecycle capability contracts.

Exit: multiple offerings of each protocol work, repeated plan names across
offerings are accepted, and cross-offering requests cannot access/revoke resources.

### Phase D — shared MCP lifecycle integration

- Replace plan-ID backend lookup and full snapshots with the Phase A contracts.
- Support multiple named backends/targets per plan, explicit target membership,
  per-target parameters and independent authentication modes.
- Preserve live authorization, generation-specific identities and capsule hooks.
- Keep capsule attribution provider-neutral and tied to the selected identity
  used for the operation. Never record assertions, tokens, secrets or
  credential-derived fingerprints; selection is not proof of authentication.

Exit: restart recovery, orphan cleanup, secret rotation and stale-replica
revocation pass against gateway-owned state.

### Phase E — delivery and documentation

- Update `cf/README.md`, `cf/configuration.md`, `cf/verification.md`, architecture,
  federation/capsule docs, development scripts and acceptance fixtures.
- Correct stale management-token descriptions while updating architecture docs.
- Update release archive contents and workflow checks; include conversion tooling
  or explicitly declared dependencies rather than relying on local installations.
- Provide deployment-vars examples separately from the two application files.
- Document accounting limits, binding credential refresh, identity prerequisites,
  configuration changes requiring migration and the no-instance rollout path.

## 8. Verification matrix

### Configuration and catalog

- YAML booleans/numbers/decimal strings/PEM values survive conversion correctly.
- Invalid or ambiguous YAML and invalid envelopes stop before CF changes.
- Native provider options pass through unchanged; named references resolve.
- Catalog contains multiple LLM and MCP offerings with their respective plans.
- Duplicate IDs, duplicate local plan names, mixed protocols, unknown references
  and invalid schemas are rejected; cross-offering plan-name reuse is accepted.
- Required `client_id` is advertised and enforced. Missing, malformed, unknown
  and binding-time identity parameters are rejected.

### Lifecycle and isolation

- Provision/bind/unbind/deprovision for every configured offering and plan.
- Wrong service/plan combinations cannot bind, mutate or delete another instance.
- Identical provision retries recover the original identity; changed parameters
  conflict rather than silently selecting a new upstream principal.
- Restart and alternate broker replicas recover entirely from gateway resources.
- Lost management responses, delayed writes and supported concurrent requests
  preserve idempotency; orphan unbind works after failed bind publication.
- New binding GUIDs receive fresh generation/session identities; old routes and
  cached configuration fail authorization after revocation.
- Deprovision revokes parent access before idempotent child cleanup.
- LLM permissions and budgets remain stable during provider credential changes.
- MCP targets with distinct selected client IDs never share identity-sensitive
  sessions or token cache entries across principals/bindings.
- Adding a target or broadening an auth scope does not silently expand existing
  instance grants; removed/missing resources fail closed.

### Integration and release

- Test federated/static/no-upstream-auth targets with controlled upstreams.
  Caller authorization remains mandatory in every mode.
- Verify management access is broker-only, provider secrets are absent from broker
  config/instance/grant records, and capsules contain only allowed attribution.
- Run native gateway contract tests plus affected Go package tests/race checks,
  vet, converter tests and CF acceptance coverage. Use existing Ginkgo reporting
  conventions when running affected Ginkgo suites.
- Azure DevOps smoke test: verified FIC/scope, initialize and a permitted tool
  call; capture sanitized outcomes. Do not claim this is verified by unit tests.
- Extract a release archive and exercise its documented conversion/deploy workflow
  to catch missing assets and undeclared tool dependencies.

## 9. Production rollout and completion criteria

1. Confirm production still has no instances before replacing service/plan
   ownership. If that changed, stop and design a staged migration instead.
2. Prepare both YAML files from current operator settings without committing real
   credentials. Preserve installation/session/signing identities.
3. Validate native configuration, references, parameter contracts, model pricing
   identifiers and CF management connectivity in the lab.
4. Deploy compatible gateway/broker/sidecar binaries and configuration in a
   coordinated sequence; install shared gateway resources before offering new
   catalog functionality. Check replica readiness before enabling access.
5. Remove obsolete application variables, refresh the registered CF broker
   catalog and enable the intended offerings/plans.
6. Smoke-test inference and MCP lifecycle, then verify restart/revocation.
7. Record rollback requirements. Once new managed resource formats or instances
   exist, reverting binaries alone is not necessarily safe; require tested format
   compatibility or clean up test instances before rollback.

Complete when operators maintain two readable application YAML files, deployment
still uses manifest vars interpolation, offerings are independent, upstream MCP
identity is selected at instance creation, and all lifecycle guarantees continue
to derive from gateway-owned resources.
