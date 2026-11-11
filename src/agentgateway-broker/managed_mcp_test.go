package main

import (
	mcpconfig "cf-mcp-config"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMCPProvisionRetryUsesPersistedSnapshot(t *testing.T) {
	for _, change := range []string{"tighter parameter validation", "different auth method"} {
		t.Run(change, func(t *testing.T) {
			b, _ := fixture(t)
			backend := mcpconfig.Backend{Name: "work", PlanID: "work-plan", URL: "https://tools.test/mcp", Auth: mcpconfig.Auth{
				Method: "federated_client_credentials", Issuer: "https://uaa.test/oauth/token", JWKSURL: "https://uaa.test/keys", Audience: "urn:exchange",
				TokenEndpoint: "https://tokens.test/{tenant}/token", Scopes: []string{"read"}, Parameters: map[string]string{"tenant": "[a-z]+"},
			}}
			request := Request{ServiceID: "service", PlanID: "work-plan", Parameters: map[string]any{"client_id": "client", "tenant": "team"}}
			instance := mcpconfig.Instance{ID: "one", Installation: b.cfg.Installation, ServiceID: request.ServiceID, PlanID: request.PlanID, Fingerprint: fingerprint(request), Backend: backend, Generation: "persisted-generation", Parameters: map[string]string{"client_id": "client", "tenant": "team"}}
			metadata, err := json.Marshal(instance)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("retry attempted a management write: %s", r.Method)
					w.WriteHeader(500)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/mcp.instance") {
					write(w, 200, map[string]any{"resources": []any{map[string]any{"id": managedID(b.cfg.Installation, "one", ""), "value": managedValue{Name: managedID(b.cfg.Installation, "one", ""), Metadata: metadata}}}})
					return
				}
				write(w, 200, map[string]any{"resources": []any{}})
			}))
			defer server.Close()
			// Use a fresh replica and a separately decoded backend, so editing current
			// configuration cannot mutate the persisted snapshot's parameter map.
			data, _ := json.Marshal(backend)
			var current mcpconfig.Backend
			if err := json.Unmarshal(data, &current); err != nil {
				t.Fatal(err)
			}
			if change == "tighter parameter validation" {
				current.Auth.Parameters["tenant"] = "production"
			} else {
				current.Auth = mcpconfig.Auth{Method: "none", Issuer: backend.Auth.Issuer, JWKSURL: backend.Auth.JWKSURL, Audience: backend.Auth.Audience}
			}
			b.cfg.MCPServers = []mcpconfig.Backend{current}
			b.cfg.MCPURL = "https://gateway.test/mcp"
			b.cfg.ManagementURL = server.URL
			replica, err := newBroker(b.cfg)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(request)
			if w := call(replica, "PUT", "/v2/service_instances/one", string(body)); w.Code != 200 {
				t.Fatalf("identical retry: %d %s", w.Code, w.Body)
			}
			request.Parameters["tenant"] = "different"
			conflict, _ := json.Marshal(request)
			if w := call(replica, "PUT", "/v2/service_instances/one", string(conflict)); w.Code != 409 {
				t.Fatalf("conflicting retry: %d %s", w.Code, w.Body)
			}
			if w := call(replica, "PUT", "/v2/service_instances/new", string(body)); w.Code != 400 {
				t.Fatalf("new instance must use current validation: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestMCPTranslatorRecoversFromGatewayStateWithoutDatabase(t *testing.T) {
	b, _ := fixture(t)
	config := `{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":{"method":"passthrough","issuer":"https://uaa.test/oauth/token","jwks_url":"https://uaa.test/token_keys","audience":"urn:test:exchange"}}]}`
	if err := json.Unmarshal([]byte(config), &b.cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	resources := map[string]map[string]json.RawMessage{}
	deleted := map[string]bool{}
	loseResponse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/config/resources/"), "/")
		kind := parts[0]
		if resources[kind] == nil {
			resources[kind] = map[string]json.RawMessage{}
		}
		if r.Method == http.MethodGet {
			var list []any
			for id, value := range resources[kind] {
				list = append(list, map[string]any{"id": id, "value": value})
			}
			write(w, 200, map[string]any{"resources": list})
			return
		}
		if r.Method == http.MethodDelete {
			if len(parts) != 2 {
				w.WriteHeader(400)
				return
			}
			delete(resources[kind], parts[1])
			deleted[kind+"/"+parts[1]] = true
			write(w, 200, map[string]any{})
			return
		}
		var body struct {
			Resources []struct {
				Value json.RawMessage `json:"value"`
			} `json:"resources"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		for _, v := range body.Resources {
			var value struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(v.Value, &value) != nil || value.Name == "" {
				w.WriteHeader(400)
				return
			}
			if deleted[kind+"/"+value.Name] {
				w.WriteHeader(409)
				return
			}
			if old, ok := resources[kind][value.Name]; ok && string(old) != string(v.Value) {
				w.WriteHeader(409)
				return
			}
			resources[kind][value.Name] = v.Value
		}
		if loseResponse {
			loseResponse = false
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		}
		write(w, 200, map[string]any{})
	}))
	defer server.Close()
	b.cfg.ManagementURL = server.URL
	provision := `{"service_id":"service","plan_id":"work-plan"}`
	w := call(b, "PUT", "/v2/service_instances/one", provision)
	if w.Code != 201 {
		t.Fatalf("provision: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if len(resources["mcp.instance"]) != 1 {
		t.Error("instance selection was not persisted through gateway management")
	}
	mu.Unlock()
	bind := `{"service_id":"service","plan_id":"work-plan","bind_resource":{"app_guid":"app"},"context":{"platform":"cloudfoundry","service_account":{"issuer":"https://uaa.test/oauth/token","subject":"cf:service-account:team:worker"}}}`
	path := "/v2/service_instances/one/service_bindings/binding-one"
	mu.Lock()
	loseResponse = true
	mu.Unlock()
	w = call(b, "PUT", path, bind)
	if w.Code != 503 {
		t.Fatalf("expected uncertain grant write: %d %s", w.Code, w.Body)
	}
	// Another broker replica has only operator configuration and API credentials.
	other, err := newBroker(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	w = call(other, "PUT", path, bind)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("restart retry: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if len(resources["mcp.grant"]) != 1 {
		t.Errorf("retry produced %d grants", len(resources["mcp.grant"]))
	}
	mu.Unlock()
	if !strings.Contains(w.Body.String(), "/mcp/one/binding-one") || strings.Contains(w.Body.String(), "api_key") {
		t.Fatalf("credentials: %s", w.Body)
	}
	w = call(other, "DELETE", path+"?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 && w.Code != 410 {
		t.Fatalf("unbind: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if len(resources["mcp.grant"]) != 0 {
		t.Error("unbind left gateway grant")
	}
	mu.Unlock()
	// CAPI can orphan-unbind a bind whose response was never received.
	mu.Lock()
	loseResponse = true
	mu.Unlock()
	orphan := "/v2/service_instances/one/service_bindings/orphan"
	w = call(b, "PUT", orphan, bind)
	if w.Code != 503 {
		t.Fatalf("orphan setup: %d %s", w.Code, w.Body)
	}
	w = call(other, "DELETE", orphan+"?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 && w.Code != 410 {
		t.Fatalf("orphan cleanup: %d %s", w.Code, w.Body)
	}
	// Clean-up of an unknown ID must also fence a delayed initial bind.
	late := "/v2/service_instances/one/service_bindings/late"
	w = call(other, "DELETE", late+"?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 && w.Code != 410 {
		t.Fatalf("unknown orphan cleanup: %d %s", w.Code, w.Body)
	}
	w = call(b, "PUT", late, bind)
	if w.Code != 503 && w.Code != 409 {
		t.Fatalf("deleted binding was resurrected: %d %s", w.Code, w.Body)
	}
	w = call(b, "PUT", "/v2/service_instances/one/service_bindings/second", bind)
	if w.Code != 201 {
		t.Fatalf("new binding GUID: %d %s", w.Code, w.Body)
	}
	w = call(other, "DELETE", "/v2/service_instances/one?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 {
		t.Fatalf("deprovision: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(resources["mcp.instance"]) != 0 || len(resources["mcp.grant"]) != 0 {
		t.Error("deprovision left lifecycle resources")
	}
}
