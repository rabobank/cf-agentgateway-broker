# Management identity-route lab verification

Verified on 2026-11-16 against `https://api.10.246.0.21.sslip.io`, CAPI
3.226.0, org `poc`, space `demo`, for issue #11.

## Implemented deployment and end-to-end results

The tokenless implementation was subsequently deployed to the same lab using
`cf/deploy`, including a second invocation against the existing identity route
to verify repeat deployment. Both apps have no `MANAGEMENT_TOKEN`. The gateway
has two healthy replicas, fixed `RUST_MIN_STACK=16777216`, and only its public
inference route and `agentgateway-management.apps.identity`. The broker has no
direct gateway network policy. Its `/ready` returns 200.

The final end-to-end run used the OpenCode-configured provider `dgxSparkDm`, model
`qwen3.8-27b-nvfp4-dflash2`, at `192.168.2.148:8001`. The lab security group
`agentgateway-qwen-egress` allows that address and port in space `demo`. The
published model alias and service plan were preserved; this is operator provider
configuration, not a broker default. Provider authentication was resolved from
the referenced environment variable without printing the key. Pricing remains
the existing operator-supplied catalog, not independently verified Qwen pricing.

Passed against the deployed broker and real Qwen provider:

- CAPI service provisioning and service-key creation.
- `/v1/models` and a successful `/v1/chat/completions` response.
- Idempotent provisioning and orphan unbind followed by a new binding.
- Broker restart recovering identical credentials from gateway resources.
- Two-replica gateway rolling restart preserving management and inference access.
- Public management runtime/resource paths rejected both without authentication
  and with a valid inference credential, after management token removal.
- Service-key unbind preserving the shared instance key; deprovision revoking it;
  repeated deprovision returning 410.
- Broker instance identity reaching tokenless management (200); gateway app
  identity rejected (403); missing client certificate rejected during TLS.
- Removing the broker route policy made three successive `/ready` requests
  return 503, including use of the running broker's pooled management client.
  Restoring the policy recovered access.

Earlier runs immediately after a gateway restart observed a management DELETE
timeout with an uncertain write outcome; retry recovered and revocation was
verified. The final real-provider run completed without that retry. No automatic
management-write retry was introduced.

Unit tests with the race detector and `go vet` passed for the broker and launcher.
The broker TLS test covers CA file/directory trust, missing client identity,
untrusted servers, credential-file rotation and recovery after an invalid rotated
key. Both Docker-based lifecycle/MCP acceptance specs passed; report:
`/tmp/opencode/issue11-acceptance.json`.

The temporary provider fixture and all test service instances were removed.
The lab intentionally retains the deployed mTLS architecture and Qwen provider.
The sections below describe the initial spike, before implementation.

## Confirmed architecture

```text
broker app
  CF_INSTANCE_CERT + CF_INSTANCE_KEY
  verified server TLS using the identity-domain CA
    -> https://<management-host>.apps.identity:443
    -> Gorouter mTLS authentication and broker-app route policy
    -> gateway web destination port 9090
    -> gateway management API
```

The identity domain is an HTTP-routing domain, with
`enforce_route_policies: true` and `route_policies_scope: any`. The latter permits
different policy source scopes; the tested policy allows only the broker app GUID,
not every authenticated app.

The wildcard hostname resolved from the broker container to `10.246.0.21`.
Port 443 accepted TLS; port 9090 on that address refused connections. The gateway
destination port must not be copied into the management URL.

## Route setup used

The installed lab CLI supports these commands:

```sh
cf target -o poc -s demo
cf map-route agentgateway apps.identity --hostname issue11-management-spike --app-port 9090
cf add-route-policy apps.identity --hostname issue11-management-spike --source-app agentgateway-broker
cf restart agentgateway --strategy rolling
```

CAPI reported destination `port: 9090`, `protocol: http1`, process `web`.
Before the rolling restart, existing gateway containers exposed only port 8080
and the new route returned Gorouter `404 unknown_route`. After the restart, the
route reached management. Map additional ports before starting new deployments;
existing deployments need replacement containers when adding the port.

Manifest support for destination ports and policy declarations was not tested.
Deployment automation can use the verified CLI/CAPI operations and must reconcile
the policy source when the broker app GUID changes.

## TLS trust

Probes ran inside app containers using OpenSSL, with server hostname verification
and certificate verification enabled. Client credentials were read directly from
`CF_INSTANCE_CERT` and `CF_INSTANCE_KEY`; no private keys or tokens were copied out.

Default trust failed with `unable to get local issuer certificate`. The identity
CA was available at `/etc/cf-system-certificates/trusted-ca-4.crt`, beneath
`CF_SYSTEM_CERT_PATH`. That file's numbered name is a lab observation, not a
portable configuration contract. Explicitly trusting it validated the
`*.apps.identity` server certificate. The broker needs management CA configuration
or support for the platform-mounted trust directory, separate from inference
trust returned to consumers.

## Access results

| Request to the identity route | Result |
| --- | --- |
| Broker instance certificate, existing management token | `200` from `/api/runtime` |
| Broker instance certificate, no token | `401` from the existing gateway API-key policy |
| No client certificate | TLS handshake rejected |
| Gateway app instance certificate, with or without its management token | `403` |
| Broker certificate and token after deleting its route policy | `403` |
| Broker certificate and token with broker-to-gateway network policy removed | `200` |

Five fresh TLS connections to `/api/config/resources/llm.apiKey` also returned
`200`. Responses were filtered to status lines to avoid exposing managed resources.
Removing the route policy demonstrated default denial and revocation for new
connections. Existing keep-alive connections were not tested.

The public inference route returned `401` for `/api/runtime`, even with the
management token. This confirms rejection, but does not by itself establish that
all management paths remain inaccessible after removing management authentication.

## Implementation work identified by the initial spike

- Add management client mTLS, including instance-certificate rotation handling.
- Wire management server trust through the CF launcher.
- Automate route destination and broker-only policy creation/reconciliation.
- Remove management token authentication only after that path is established.
- Verify public-route isolation after token removal, full OSB lifecycle behavior,
  restart recovery, and access revocation with reused connections.
- Preserve the manifest's fixed `RUST_MIN_STACK: "16777216"`.

## Cleanup

The temporary identity route and its policy were deleted. The original
broker-to-gateway TCP/9090 network policy was restored. The two gateway instances
were rolled again to restore the original exposed-port configuration and reported
`2/2` running and ready. The original CLI target (`poc / qualified-two`) was
restored. No token, application configuration, or application binary was changed.
