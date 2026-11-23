package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorGatewayAssemblyKeepsProvidersAndHidesWiring(t *testing.T) {
	t.Setenv("VCAP_SERVICES", `{"postgres":[{"name":"state","credentials":{"uri":"postgres://db/gateway"}}]}`)
	raw := `{"database":{"service_name":"state"},"llm":{"providers":[{"name":"azure","provider":"azure","params":{"apiKey":"private-key","azureResourceName":"resource"}}],"models":[{"name":"chat","provider":{"reference":"azure"}}]},"model_catalog":{"providers":{}},"mcp":{"session_key":"` + strings.Repeat("12", 32) + `","caller_auth":{"issuer":"https://uaa.test","audiences":["exchange"],"jwks":{"url":"https://uaa.test/keys"}},"backends":[{"name":"tools","revision":"v1","mcp":{"targets":[{"name":"work","mcp":{"host":"https://tools.test/mcp"},"policies":{"backendAuth":{"oauthTokenExchange":{"clientAuth":{"clientId":{"fromInstanceParameter":"client_id"}}}}}}]}}]},"capsules":{"log_id":"lab","signing_seed":"` + strings.Repeat("34", 32) + `"}}`
	cfg, err := operatorGateway([]byte(raw), 8123)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["gateways"].(map[string]any)["inference"].(map[string]any)["port"] != float64(8123) {
		t.Fatal("CF port not injected")
	}
	if !strings.Contains(string(data), `"reference":"azure"`) || !strings.Contains(string(data), `"apiKey":"private-key"`) {
		t.Fatal("native configuration lost")
	}
	if len(got["mcp"].(map[string]any)["targets"].([]any)) != 0 {
		t.Fatal("operator backend publicly exposed")
	}
	if len(got["mcpBackends"].([]any)) != 1 {
		t.Fatal("shared configuration unavailable to gateway")
	}
	if strings.Contains(string(data), "34343434") {
		t.Fatal("capsule signing key forwarded to native config")
	}
}

func TestOperatorGatewayValidatesUnusedMCPTemplates(t *testing.T) {
	t.Setenv("VCAP_SERVICES", `{"postgres":[{"name":"state","credentials":{"uri":"postgres://db/gateway"}}]}`)
	for _, target := range []string{
		`{"name":"work","mcp":{"host":"https://tools.test"},"policies":{"backendAuth":{"oauthTokenExchange":{"clientAuth":{"clientId":{"fromInstanceParameter":""}}}}}}`,
		`{"name":"work","mcp":{"host":"https://tools.test"},"policies":{"requestHeaderModifier":{"set":{"X-Key":{"fromInstanceParameter":"secret"}}}}}`,
	} {
		raw := `{"database":{"service_name":"state"},"mcp":{"session_key":"` + strings.Repeat("12", 32) + `","caller_auth":{"issuer":"https://uaa.test","audiences":["exchange"],"jwks":{"url":"https://uaa.test/keys"}},"backends":[{"name":"tools","revision":"v1","mcp":{"targets":[` + target + `]}}]}}`
		if _, err := operatorGateway([]byte(raw), 8080); err == nil {
			t.Fatal("unsupported identity substitution accepted")
		}
	}
}

func TestTeamOAuthOperatorWiresOnlyExplicitToolsAndLoopbackProxy(t *testing.T) {
	t.Setenv("VCAP_SERVICES", `{"postgres":[{"name":"state","credentials":{"uri":"postgres://db/gateway"}}]}`)
	storageKey := strings.Repeat("34", 32)
	raw := `{"database":{"service_name":"state"},"mcp":{"session_key":"` + strings.Repeat("12", 32) + `","oauth_updates":{"encryption_key":"` + storageKey + `"},"backends":[{"name":"design","revision":"v1","team_oauth":{"allowed_tools":["get_metadata"]},"mcp":{"targets":[{"name":"tools","mcp":{"host":"https://mcp.test/mcp"}}]}}]}}`
	cfg, err := operatorGateway([]byte(raw), 8123)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	for _, expected := range []string{"127.0.0.1:50053/upstream/design", `"teamOAuth":true`, `"allowedTools":["get_metadata"]`} {
		if !strings.Contains(string(data), expected) {
			t.Errorf("missing %s", expected)
		}
	}
	if strings.Contains(string(data), storageKey) {
		t.Fatal("operator OAuth configuration leaked into native routing")
	}
	for _, replacement := range []string{`"allowed_tools":[]`, `"allowed_tools":["*"]`} {
		if _, err := operatorGateway([]byte(strings.Replace(raw, `"allowed_tools":["get_metadata"]`, replacement, 1)), 8123); err == nil {
			t.Fatal("invalid tool allowlist accepted")
		}
	}
}

func TestOperatorBrokerPreservesConsumerTrustAndRejectsLegacyFields(t *testing.T) {
	t.Setenv("MANAGEMENT_URL", "")
	t.Setenv("MANAGEMENT_TRUSTED_CA", "")
	raw := `{"installation":"lab","auth":{"username":"broker","password":"secret"},"gateway":{"management_url":"http://management.test"},"services":[{"id":"team","name":"team","description":"Team","protocol":"llm","connection":{"url":"https://team.test/v1","trusted_ca":"PEM"},"plans":[]}]}`
	config, err := operatorBroker([]byte(raw), 8123)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	if !strings.Contains(string(data), `"trusted_ca":"PEM"`) {
		t.Fatal("offering trust lost")
	}
	if _, err := operatorBroker([]byte(strings.Replace(raw, `"installation":"lab"`, `"installation":"lab","unknown":"secret"`, 1)), 8123); err == nil {
		t.Fatal("unknown operator fields accepted")
	}
}
