# Operator configuration implementation verification

Branch: `feat/operator-config-multi-offerings`, based on `origin/master`
`4c0679a` (pulled with fast-forward-only before branching).

## TDD history

- `8de6ff4`: failing catalog/ownership and converter contracts, plus plan and
  gateway red-test submodule commit.
- `95d50af`: failing gateway assembly contract (missing implementation).
- `2debdf6`: failing duplicate access/allowance and identity-template checks.
- `4ce901d`: failing cross-file reference validation contract.

## Passed locally

- Complete `scripts/test`, including Go converter tests/example reference
  validation, Go race tests and vet for broker/config/launcher, PostgreSQL capsule
  contracts, capsule plugin suite and existing pinned-gateway integration suite.
- Standalone compilation/execution of the new Rust `shared_mcp.rs` test with the
  pinned Rust 1.99 container and vendored build artifacts: identity resolution,
  generation-specific targets, shared credential rotation and incompatible/missing
  backend rejection.
- Acceptance package compilation (`go test ./... -run '^$'`).
- Standalone Go deployment workflow contracts and `git diff --check`.

## Standalone deployment tools

`config-vars` and `deploy` are built from `src/cf-launcher/cmd/` and shipped as
standalone Go executables. The Python converter/helper and PyYAML dependency have
been removed. Red-test commits `5232c08` and `bcbd0da` establish conversion,
cross-file validation, CF command ordering, route isolation and safe failure
contracts before the Go implementations.

## Outstanding before deployment

- Full gateway crate compilation/test execution could not complete: rustc was
  killed with SIGKILL under available host memory, including one-job and reduced
  frontend-parallelism attempts. Standalone module success is not a substitute
  for the complete gateway build/materialization tests.
- Rebuild the deployment bundle and run full acceptance, including the new
  consolidated-config scenario. Existing integration tests use existing gateway
  artifacts and do not verify the new Rust materialization path end to end.
- Publish the local gateway submodule branch `feat/cf-operator-config` to its fork
  before CI/other checkouts consume the new pinned commit. Nothing was pushed.
- Run a real Azure DevOps WIF smoke test with confirmed hosted MCP scope/FIC
  settings. The referenced gist reports success but leaves those details open.

## Remaining hardening from the broader plan

- Native policies in unreferenced MCP templates receive envelope/marker checks,
  but complete native validation happens when gateway routes are materialized.
- MCP revision compatibility is explicitly operator-managed. Increment revisions
  for permission/auth/target changes; do not treat same-revision arbitrary edits
  as automatic safe migrations. Static file changes roll out per replica.
- Legacy runtime configuration and its acceptance scenario remain supported for
  development compatibility. The CF manifest and new examples use consolidated
  configuration; older detailed docs are labeled as legacy.
