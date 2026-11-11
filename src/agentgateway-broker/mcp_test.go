package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMCPCatalogAndInstanceSelection(t *testing.T) {
	b, _ := fixture(t)
	if err := json.Unmarshal([]byte(`{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":{"method":"federated_client_credentials","issuer":"https://uaa.test/oauth/token","jwks_url":"https://uaa.test/token_keys","audience":"urn:test:exchange","token_endpoint":"https://login.test/{tenant}/token","scopes":["urn:test:resource"],"parameters":{"tenant":"^[a-z0-9-]+$"}}}]}`), &b.cfg); err != nil {
		t.Fatal(err)
	}
	w := call(b, http.MethodGet, "/v2/catalog", "")
	var catalog struct {
		Services []struct {
			Plans []struct {
				ID string `json:"id"`
			} `json:"plans"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Services) != 1 || len(catalog.Services[0].Plans) != 2 || catalog.Services[0].Plans[1].ID != "work-plan" {
		t.Fatalf("unexpected catalog: %s", w.Body)
	}
}
func TestMCPRejectsUnknownOrUnsafeAuthConfiguration(t *testing.T) {
	for _, auth := range []string{`{"method":"invented"}`, `{"method":"federated_client_credentials","issuer":"https://uaa.test","jwks_url":"https://uaa.test/keys","audience":"urn:exchange","token_endpoint":"https://{tenant}/token","parameters":{"tenant":".*"},"scopes":["read"]}`} {
		b, _ := fixture(t)
		if err := json.Unmarshal([]byte(`{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":`+auth+`}]}`), &b.cfg); err != nil {
			t.Fatal(err)
		}
		if err := b.cfg.validate(); err == nil {
			t.Fatalf("unsafe auth accepted: %s", auth)
		}
	}
}
func TestMCPCatalogPlanDescriptions(t *testing.T) {
	for _, test := range []struct{ name, description, want string }{
		{"omitted", "", "Access to the work MCP backend"},
		{"whitespace", " \t\n", "Access to the work MCP backend"},
		{"custom", "Team-owned read-only tools", "Team-owned read-only tools"},
		{"preserves custom formatting", " Team tools ", " Team tools "},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := fixture(t)
			config := map[string]any{"mcp_url": "https://gateway.test/mcp", "mcp_servers": []any{map[string]any{"name": "work", "plan_id": "work-plan", "url": "https://tools.test/mcp"}}}
			if test.description != "" {
				config["mcp_servers"].([]any)[0].(map[string]any)["description"] = test.description
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &b.cfg); err != nil {
				t.Fatal(err)
			}
			response := call(b, http.MethodGet, "/v2/catalog", "")
			var catalog struct {
				Services []struct {
					Plans []struct {
						ID          string `json:"id"`
						Description string `json:"description"`
					} `json:"plans"`
				} `json:"services"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || len(catalog.Services) != 1 || len(catalog.Services[0].Plans) != 2 {
				t.Fatalf("unexpected catalog: %d %s", response.Code, response.Body)
			}
			plan := catalog.Services[0].Plans[1]
			if plan.ID != "work-plan" || plan.Description != test.want {
				t.Fatalf("plan %q description = %q, want %q", plan.ID, plan.Description, test.want)
			}
		})
	}
}
