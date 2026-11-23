# Team-shared hosted MCP with CF CLI OAuth

## CLI setup

1. Register an approved public OAuth client for the hosted MCP server. The CF
   plugin uses authorization code with S256 PKCE and a loopback redirect of
   `http://127.0.0.1:<port>/callback`. Set a fixed `callback_port` if the provider
   requires it. The client must be able to exchange the code without a secret.
2. Set `mcp.oauth_updates.encryption_key` to a random 32-byte hex storage
   key in `gateway.yml`. Share it across gateway replicas. Configure a backend
   with one HTTPS MCP target and a nonempty `team_oauth.allowed_tools` list.
3. Configure `team_oauth.binding_key` in `broker.yml` and share it across
   broker replicas. Add a single-backend MCP
   plan with `oauth_profile`: `version: 1`, `resource` (the exact MCP URL),
   `issuer`, `client_id`, `scopes`, and optional `callback_port`. After changing
   profile metadata, run `cf update-service-broker` to refresh CAPI's catalog.
4. Install `dist/cf-agentgateway` from the build. After targeting the desired
   space, run `cf ag list`, `cf ag connect INSTANCE`, `cf ag status INSTANCE`,
   or `cf ag disconnect INSTANCE`. To replace a working token, use `cf ag
   connect INSTANCE --replace`. `agentgateway` is the unabbreviated command.

The CLI uses `cf curl` to read plan metadata and status. It sends the token in
the service-instance update body over HTTPS using the current CF login. CAPI
and the broker handle the token as a secret request parameter. The broker
forwards it over the restricted management route; only the gateway persists it,
encrypted at rest. The CLI does not pass the update body through `cf curl` or
`CF_TRACE`.
The instance generation and connection sequence fence stale updates. The
latest identical operation is safe to retry. Status contains no credentials.
There is no refresh-token exchange: reauthorize when the access token expires.

### Local acceptance

With CF CLI v8, Docker, and Devbox installed, run `devbox run build` followed by
`devbox run acceptance`. To run just the CLI spec, pass `--label-filter=cf-plugin`
to the acceptance Ginkgo CLI. The spec uses the installed CF CLI and an isolated
CF home, a Go CAPI stub, and the `oauth-pkce-mcp` HTTPS fixture. It runs the real
broker, Agentgateway, and PostgreSQL. There is no live SaaS login or full CF
deployment. CI requires the CF CLI; locally the plugin spec is skipped if `cf`
is missing.

## Figma and agent bindings

Use the hosted resource `https://mcp.figma.com/mcp`, an approved client, and
the `mcp:connect` scope. See the [Figma hosted setup guide](https://developers.figma.com/docs/figma-mcp-server/remote-server-installation/).
Start the tool allowlist with `get_metadata` and `get_screenshot`.

Bind the team agent's CF app and configure its MCP client with the binding's
`mcp_url` and `headers`. Disconnect disables the gateway connection; it does
not revoke the token at Figma. Replacing the authorization invalidates previous
upstream MCP session IDs.

Only files suitable for all team members should be accessible to the connected
identity. A read-tool allowlist constrains operations, not file visibility.
The gateway filters discovery and invocation, and the upstream connector
rejects unlisted calls and other action families. The connector supports
Streamable HTTP POSTs (including responses streamed over SSE), not standalone
upstream GET streams.
