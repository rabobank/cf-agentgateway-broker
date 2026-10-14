# MVP architecture

## Components

CF Cloud Controller calls the stateless Go OSB broker over HTTPS/Basic auth.
The broker reads/writes private agentgateway management API resources. Gateway
resources in shared PostgreSQL are authoritative for instances and credentials.

CF applications receive `{url, api_key}` and call the BOSH DNS inference alias.
Multiple source-built agentgateway replicas authenticate the shared instance key,
restrict models and account for USD usage. No UAA, ext_authz bridge, or load balancer.

The broker/PostgreSQL instance group is singleton initially, with persistent disk
for PostgreSQL. The gateway group is horizontally scalable; both app jobs use BPM.

## Resource ownership and lifecycle

Each instance maps to a random gateway key resource. Custom metadata labels carry
installation identity, instance UUID, org/space, service/plan IDs and provision
fingerprint. The gateway assigns its own resource UUID; the broker discovers it
by listing/filtering metadata. Raw keys are stored/recovered through that private API.

Provision first looks up the instance and compares the fingerprint. Bind/service
key returns its shared credential. Unbind preserves the key; deprovision removes
it. Duplicate metadata matches fail rather than silently choosing a resource.
One immutable operator plan supplies explicit models, USD allowance and duration.
The gateway key resource snapshots its enforcement policy at provision.

OSB 2.15 and newer compatible 2.x, synchronous operations, no detached workflows, no custom parameters,
plan updates or advertised retrieval. Relies on CAPI serialization of same-instance
requests. Delayed/ambiguous remote writes remain a retry test case; this is not a
general-purpose concurrent control plane with atomic create/CAS support.

## Replica behavior and accounting

PostgreSQL LISTEN/NOTIFY prompts configuration reload across replicas. Activation
and revocation are eventual. Budget flushes atomically add deltas and reconcile
shared totals on a five-second cadence; admission uses local memory. Charges occur
after responses. Streams/concurrent requests and synchronization can overshoot;
crashes can lose unflushed usage. Database outages do not guarantee fail-closed
admission. Missing usage/pricing can leave requests uncharged.

Counter identity is key hash plus budget name. Keep keys, budget names, units and
windows stable across reconciliation. Budget windows are epoch-aligned, not calendar
months or durations starting at provision. Price only explicitly verified models.

## Networking and packaging

Inference and authenticated management use distinct HTTPS listeners. Raw admin
stays loopback-only. Network access to management is private. Certificates cover
BOSH DNS aliases; CF consumers must trust the CA and resolve/reach those names.

Pin main source and compiler revisions; compile without UI assets. Vendor Cargo
registry/Git sources before offline BOSH compilation. Local/LFS blobstore is
temporary; do not commit blob objects before permanent blobstore configuration.

See [source evidence](source-investigation.md) and [operations](operator-runbook.md).
