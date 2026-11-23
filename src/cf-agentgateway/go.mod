module cf-agentgateway-cli

go 1.26.9

require (
	cf-mcp-config v0.0.0
	code.cloudfoundry.org/cli/v8 v8.19.1
)

replace cf-mcp-config => ../mcpconfig
