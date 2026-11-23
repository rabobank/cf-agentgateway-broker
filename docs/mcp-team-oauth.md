# Team-shared hosted MCP with CF CLI OAuth

## CLI setup

1. Register an approved public OAuth client for the hosted MCP server. The CF
   plugin uses authorization code with S256 PKCE and a loopback redirect of
   `http://127.0.0.1:<port>/callback`. Set a fixed `callback_port` if the provider
   requires it. The client must be able to exchange the code without a secret.
2. Set `mcp.oauth_updates.private_key` to a 32-byte hex X25519 private key and
   `mcp.oauth_updates.encryption_key` to a separate random 32-byte hex storage
   key in `gateway.yml`. Share both across gateway replicas. Configure a backend
   with one HTTPS MCP target and a nonempty `team_oauth.allowed_tools` list.
3. Configure `team_oauth.binding_key` in `broker.yml` and add a single-backend MCP
   plan with `oauth_profile`: `version: 1`, `resource` (the exact MCP URL),
   `issuer`, `client_id`, `scopes`, the matching X25519 `public_key` (hex raw
   bytes), and optional `callback_port`. Use a separate binding key across all
   replicas of the same broker installation. Refresh the CF broker catalog
   after changing public profile metadata. Do not configure `dashboard_url`
   or `oauth_dashboard` for CLI mode.
4. Install `dist/cf-agentgateway` from the build. After targeting the desired
   space, run `cf ag list`, `cf ag connect INSTANCE`, `cf ag status INSTANCE`,
   or `cf ag disconnect INSTANCE`. To replace a working token, use `cf ag
   connect INSTANCE --replace`. `agentgateway` is the unabbreviated command.

The CLI uses `cf curl` against the current CAPI target to read the plan metadata
and submit the update. The upstream access token is encrypted to the gateway
with HPKE before it enters CAPI. The broker translates OSB updates over its
restricted management route; only the gateway decrypts and stores credentials.
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

## Optional browser token-import pilot

A team service instance shares one upstream OAuth access token across its app
bindings. Each binding gets its own Agentgateway bearer in `credentials.headers`
and only its verifier is saved on the gateway grant. The gateway-side dashboard
uses CF OAuth and checks live CAPI `manage` permission for the service instance
before it shows or changes the connection. OAuth access tokens are encrypted in
gateway-managed configuration; the broker and agents never receive them.

## Configure the pilot

1. Register a confidential CF UAA client for the dashboard. Allow authorization
   code with PKCE and `cloud_controller_service_permissions.read`. Register the
   exact redirect `https://<gateway-host>/mcp-connections/callback`. Configure
   the public origin to match a route mapped to the gateway CF app.
2. In `gateway.yml`, add `mcp.oauth_dashboard` with `public_url`, `cf_api`, a
   random 32-byte hex `encryption_key`, and `login` with `authorization_url`,
   `token_url`, `client_id`, `client_secret`, `client_auth`
   (`client_secret_basic` or `client_secret_post`), and `scopes`. Only set
   `issuer` if UAA sends the corresponding `iss` authorization response field.
   Use the same encryption key across replicas, separately from the MCP session
   key. Treat operator config containing secrets as private.
3. Add an MCP backend with a stable `name`, `revision`, exactly one
   `mcp.targets` entry containing only `name` and `mcp.host`, and
   `team_oauth.allowed_tools` containing explicit upstream tool names. For
   Figma, the endpoint is `https://mcp.figma.com/mcp`; a narrow starting list is
   `get_metadata` and `get_screenshot`. Audit upstream tool semantics before
   adding tools and increment the backend revision on incompatible changes.
4. In `broker.yml`, set `team_oauth.dashboard_url` to the same public origin and
   `team_oauth.binding_key` to a separate random 32-byte hex secret shared by
   broker replicas. Publish an MCP plan referencing just this backend, with no
   identity parameters. Provision an instance and bind the team agent's CF app.
5. Configure the MCP client with the binding's `mcp_url` and `headers`, keeping
   the bearer out of prompts and shared workspace files. The T3 buildpack's
   automatic MCP binding consumption is separate from this release.

## Import a token

Use a Figma-approved OAuth client and obtain an access token for the hosted MCP
resource with `mcp:connect`. Reuse an existing local client if its integration
supports exporting an MCP access token for this use. The Figma desktop MCP
server at `127.0.0.1` does not itself issue hosted MCP tokens. The registration
endpoint does not imply arbitrary clients are approved; see the
[Figma hosted setup guide](https://developers.figma.com/docs/figma-mcp-server/remote-server-installation/).

Open the OSB dashboard URL, sign in with UAA, and import the access token and
its expiry (RFC 3339, at most 24 hours ahead). Importing does not refresh or
validate the token with Figma; the first MCP call verifies it upstream. Import
a replacement when the token expires. Disconnect removes its use by this
gateway instance; it does not revoke the token at the provider. A replaced
connection invalidates previous upstream MCP session IDs.

Only files suitable for all team members should be accessible to the connected
identity. A read-tool allowlist constrains operations, not file visibility.
The gateway filters discovery and invocation, and the upstream connector
rejects unlisted calls and other action families. The connector supports
Streamable HTTP POSTs (including responses streamed over SSE), not standalone
upstream GET streams.
