package mcpconfig

import (
	"strings"
	"testing"
)

func TestBackendDiagnosticsIdentifyFieldWithoutValues(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`[{"name":"tools","url":"https://user:secret-value@host:invalid"}]`, "MCP_SERVERS[0]: url"},
		{`[{"name":"tools","url":"https://tools"},{"name":"tools","url":"https://other"}]`, "MCP_SERVERS[1]: duplicate backend name"},
		{`[{"name":"tools","url":"https://tools","plan_id":"plan","auth":{"issuer":"https://issuer","jwks_url":"secret-value"}}]`, "MCP_SERVERS[0]: auth.jwks_url"},
		{`[{"name":"tools","url":"https://tools","headers":{"Authorization":42}}]`, "incorrect JSON type at byte"},
		{`[{"name":"secret-value"`, "empty or incomplete JSON"},
		{`null`, "not null"},
	} {
		_, err := Parse(test.raw)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("want %q, got %v", test.want, err)
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("configuration value leaked: %v", err)
		}
	}
}
