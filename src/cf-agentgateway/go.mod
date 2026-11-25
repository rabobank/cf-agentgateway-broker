module cf-agentgateway-cli

go 1.26.9

require (
	cf-mcp-config v0.0.0
	code.cloudfoundry.org/cli/v8 v8.19.1
	github.com/creack/pty v1.1.24
	golang.org/x/term v0.47.0
)

require golang.org/x/sys v0.49.0 // indirect

replace cf-mcp-config => ../mcpconfig
