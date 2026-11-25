# Team-shared hosted MCP with CF CLI OAuth

## CLI setup

1. Use an approved OAuth client identity for the hosted MCP server. The CF
   plugin supports a pre-registered public client ID or unauthenticated
   [Dynamic Client Registration (RFC 7591)](https://www.rfc-editor.org/rfc/rfc7591.html).
   It uses authorization code with S256 PKCE and a loopback redirect of
   `http://127.0.0.1:<port>/callback`. Set a fixed `callback_port` if the provider
   requires it. A pre-registered client must support code exchange without a secret.
2. Set `mcp.oauth_updates.encryption_key` to a random 32-byte hex storage
   key in `gateway.yml`. Share it across gateway replicas. Configure a backend
   with one HTTPS MCP target and a nonempty `team_oauth.allowed_tools` list.
3. Configure `team_oauth.binding_key` in `broker.yml` and share it across
   broker replicas. Add a single-backend MCP
   plan with `oauth_profile`: `version: 1`, `resource` (the exact MCP URL),
   `issuer`, `scopes`, and optional `callback_port`. Configure either `client_id`
   for a pre-registered client, or `registration: {client_name: APPROVED_NAME}`
   (with an optional HTTPS `client_uri`). In the latter case, the CLI discovers
   the registration endpoint and registers its exact callback for each login.
   If the provider restricts registration, use a name it has approved for this
   integration. After changing profile metadata, run `cf update-service-broker`
   to refresh CAPI's catalog.
4. Install `dist/cf-agentgateway` from the build. After targeting the desired
   space, run `cf ag list`, `cf ag connect INSTANCE`, `cf ag status INSTANCE`,
   or `cf ag disconnect INSTANCE`. To replace a working token, use `cf ag
   connect INSTANCE --replace`. `agentgateway` is the unabbreviated command.

If the browser is on a different machine from the CLI, use `cf ag connect
INSTANCE --paste` (add `--replace` if needed). Open the printed URL, approve
access, then copy the full loopback redirect URL from the browser address bar
into the hidden terminal prompt. The browser may show a connection error: paste
mode does not start a listener. It uses port 19876 unless `callback_port` is set.

For dynamic registration, the CLI uses the returned token authentication method where it agrees with
discovery and can be used with the returned credentials. It supports `none`,
`client_secret_basic`, and `client_secret_post`. For example, if registration
returns both a secret and a `none` hint but discovery advertises only Basic and
POST authentication, the CLI uses Basic. The registration secret stays in CLI
memory unless automatic refresh is enabled and the provider returns a refresh token.

The CLI uses `cf curl` to read plan metadata and status. It sends the token in
the service-instance update body over HTTPS using the current CF login. CAPI
and the broker handle the token as a secret request parameter. The broker
forwards it over the restricted management route; only the gateway persists it,
encrypted at rest. The CLI does not pass the update body through `cf curl` or
`CF_TRACE`.
The instance generation and connection sequence fence stale updates. The
latest identical operation is safe to retry. Status contains no credentials.
### Automatic token refresh (opt-in)

Set the same exact HTTPS `refresh_token_url` in the broker plan's `oauth_profile`
and the gateway backend's `team_oauth` settings. For hosted Figma, the advertised
token endpoint is `https://api.figma.com/v1/oauth/token`. Update both apps,
refresh the broker catalog, and reconnect with the updated `cf ag connect
INSTANCE --replace` (add `--paste` if needed). Existing connections do not contain
refresh credentials; no rebind is needed. The CLI verifies the discovered token
endpoint against the configured one. It sends refresh credentials through CAPI
only when configured and the provider issues a refresh token. The gateway stores
them encrypted alongside the access token. `cf ag status` shows whether automatic
refresh is enabled without exposing credentials.

Each access token is used for at most 24 hours, or less if the provider reports a
shorter `expires_in`. With refresh enabled, the gateway renews it on an MCP call
near or after expiry, or after an upstream `401`. It does not replay a rejected
tool call; retry the request once, and reopen the MCP session if the upstream
requires it. Rotated refresh tokens are saved. If no refresh token was issued,
or an exchange fails or has an uncertain outcome, reconnect manually. Figma
advertises refresh support; use status after reconnect to verify that the
particular login actually returned refresh credentials.

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
For the read-only Figma plan, set the gateway backend's `team_oauth` settings
as follows. This uses Figma's [Read tools index](https://developers.figma.com/docs/figma-mcp-server/tools-and-prompts/#read-tools)
and omits `export_video` to limit storage use in CF containers and JuiceFS.
`download_assets` remains enabled; it can return links to original assets and
rendered exports, so downloaded files still count against workspace storage.

```yaml
team_oauth:
  refresh_token_url: https://api.figma.com/v1/oauth/token
  allowed_tools:
    - download_assets
    - get_code_connect_map
    - get_code_connect_suggestions
    - get_comments
    - get_context_for_code_connect
    - get_design_context
    - get_figjam
    - get_generative_plugin
    - get_libraries
    - get_metadata
    - get_motion_context
    - get_screenshot
    - get_shader
    - get_variable_defs
    - list_file_shaders
    - list_generative_plugins
    - list_shaders
    - search_design_system
    - whoami
```

Set the same `refresh_token_url` in the broker plan's `oauth_profile` and refresh
the CF catalog after deployment. Reauthorize with the updated `cf ag connect
INSTANCE --replace` to capture any refresh token Figma issues. `tools/list`
returns the intersection of the allowlist and the tools Figma exposes to the
connected client and account; not every documented tool is guaranteed to appear.

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
