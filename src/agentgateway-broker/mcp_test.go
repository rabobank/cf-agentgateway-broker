package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMCPCatalogAndInstanceSelection(t *testing.T) {
	b, _ := fixture(t)
	// Configure through JSON so this contract fails at runtime before the new
	// configuration types exist, rather than relying on a compilation failure.
	if err := json.Unmarshal([]byte(`{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":{"method":"federated_client_credentials","issuer":"https://uaa.test/oauth/token","jwks_url":"https://uaa.test/token_keys","audience":"urn:test:exchange","token_endpoint":"https://login.test/{tenant}/token","scopes":["urn:test:resource"],"parameters":{"tenant":"^[a-z0-9-]+$"}}}]}`), &b.cfg); err != nil {
		t.Fatal(err)
	}
	w := call(b, http.MethodGet, "/v2/catalog", "")
	var catalog struct { Services []struct { Plans []struct { ID string `json:"id"` } `json:"plans"` } `json:"services"` }
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil { t.Fatal(err) }
	if len(catalog.Services) != 1 || len(catalog.Services[0].Plans) != 2 || catalog.Services[0].Plans[1].ID != "work-plan" {
		t.Fatalf("expected inference and configured MCP plan: %s", w.Body.String())
	}
}

func TestMCPRejectsUnknownOrUnsafeAuthConfiguration(t *testing.T) {
	for _, auth := range []string{
		`{"method":"invented"}`,
		`{"method":"federated_client_credentials","issuer":"https://uaa.test","jwks_url":"https://uaa.test/keys","audience":"urn:exchange","token_endpoint":"https://{tenant}/token","parameters":{"tenant":".*"},"scopes":["read"]}`,
	} {
		b, _ := fixture(t)
		if err := json.Unmarshal([]byte(`{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":`+auth+`}]}`), &b.cfg); err != nil { t.Fatal(err) }
		if err := b.cfg.validate(); err == nil { t.Fatalf("unsafe auth accepted: %s",auth) }
	}
}
