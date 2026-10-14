# MVP implementation status

## Implemented

- Pinned agentgateway main submodule and Rust/Go vendored compiler packages.
- Devbox tooling and temporary local/LFS blobstore configuration.
- Standard-library Go OSB broker with metadata lookup, shared native credentials,
  provision fingerprints, synchronous lifecycle and bounded management requests.
- Broker lifecycle/restart/conflict tests and serialized BOSH template tests.
- Headless gateway/broker packages and BPM jobs.
- Initial two-gateway, singleton broker/PostgreSQL homelab manifest and DNS aliases.
- Local two-process PostgreSQL spike harness with mock OpenAI/SSE usage.

## Verification gates

1. Local headless gateway build and API validation.
2. Two-replica PostgreSQL propagation, budget convergence and restart spike.
3. Frozen/offline dependency resolution and dev release creation.
4. Noble BOSH compilation, runtime libraries and startup.
5. CF-container DNS/TLS/network checks and real OSB lifecycle.
6. Scale-out, listener reconnect, graceful updates and ambiguous-write retries.

Verification results are recorded separately; implemented scaffolding is not proof
that every deployment gate passed. See the [runbook](operator-runbook.md).
