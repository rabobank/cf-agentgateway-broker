package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMCPPersistentBindingLifecycle(t *testing.T) {
	work := t.TempDir()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	run := func(name string, args ...string) {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
	}
	data := filepath.Join(work, "data")
	run("initdb", "-D", data, "-A", "trust", "-U", "postgres")
	run("pg_ctl", "-D", data, "-l", filepath.Join(work, "log"), "-o", fmt.Sprintf("-h 127.0.0.1 -p %d -k %s", port, work), "-w", "start")
	t.Cleanup(func() { run("pg_ctl", "-D", data, "-m", "fast", "-w", "stop") })
	state, err := openMCPState(context.Background(), fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.pool.Close)
	b, _ := fixture(t)
	b.state = state
	config := `{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":{"method":"federated_client_credentials","issuer":"https://uaa.test/oauth/token","jwks_url":"https://uaa.test/token_keys","audience":"urn:test:exchange","token_endpoint":"https://login.test/{tenant}/token","scopes":["urn:test:resource"],"parameters":{"tenant":"[a-z0-9-]+"}}}]}`
	if err = json.Unmarshal([]byte(config), &b.cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var failDelete bool
	routes := map[string]json.RawMessage{}
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(r.URL.Path, "llm.apiKey") {
			write(w, 200, map[string]any{"resources": []any{}})
			return
		}
		if r.Method == "DELETE" {
			if failDelete {
				w.WriteHeader(503)
				return
			}
			delete(routes, strings.TrimPrefix(r.URL.Path, "/api/config/resources/traffic.route/"))
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
			var route struct {
				Name string `json:"name"`
			}
			json.Unmarshal(v.Value, &route)
			routes[route.Name] = v.Value
		}
		write(w, 200, map[string]any{})
	}))
	defer management.Close()
	b.cfg.ManagementURL = management.URL
	provision := `{"service_id":"service","plan_id":"work-plan","parameters":{"client_id":"client-one","tenant":"team"}}`
	w := call(b, "PUT", "/v2/service_instances/one", provision)
	if w.Code != 201 {
		t.Fatalf("provision %d: %s", w.Code, w.Body)
	}
	w = call(b, "PUT", "/v2/service_instances/one", provision)
	if w.Code != 200 {
		t.Fatalf("retry: %s", w.Body)
	}
	bind := `{"service_id":"service","plan_id":"work-plan","bind_resource":{"app_guid":"app"},"context":{"platform":"cloudfoundry","service_account":{"issuer":"https://uaa.test/oauth/token","subject":"cf:service-account:team:worker"}}}`
	w = call(b, "PUT", "/v2/service_instances/one/service_bindings/binding-one", bind)
	if w.Code != 201 {
		t.Fatalf("bind %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "api_key") || !strings.Contains(w.Body.String(), "/mcp/one/binding-one") || !strings.Contains(w.Body.String(), "urn:test:exchange") {
		t.Fatalf("unexpected credentials: %s", w.Body)
	}
	mu.Lock()
	if len(routes) != 1 {
		t.Errorf("expected one route, got %d", len(routes))
	}
	mu.Unlock()
	var count int
	if err = state.pool.QueryRow(context.Background(), "SELECT count(*) FROM agw_mcp_bindings").Scan(&count); err != nil || count != 1 {
		t.Fatalf("grant missing: %d %v", count, err)
	}
	other := *b
	other.state, err = openMCPState(context.Background(), fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer other.state.pool.Close()
	w = call(&other, "PUT", "/v2/service_instances/one/service_bindings/binding-one", bind)
	if w.Code != 201 {
		t.Fatalf("restart retry %d: %s", w.Code, w.Body)
	}
	w = call(b, "DELETE", "/v2/service_instances/one/service_bindings/binding-one?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 {
		t.Fatalf("unbind: %s", w.Body)
	}
	if err = state.pool.QueryRow(context.Background(), "SELECT count(*) FROM agw_mcp_bindings").Scan(&count); err != nil || count != 0 {
		t.Fatalf("grant not revoked: %d %v", count, err)
	}
	w = call(b, "PUT", "/v2/service_instances/one/service_bindings/binding-one", bind)
	if w.Code != 201 {
		t.Fatalf("rebind: %s", w.Body)
	}
	mu.Lock()
	failDelete = true
	mu.Unlock()
	w = call(b, "DELETE", "/v2/service_instances/one/service_bindings/binding-one?service_id=service&plan_id=work-plan", "")
	if w.Code != 503 {
		t.Fatalf("expected cleanup failure: %s", w.Body)
	}
	var revoked bool
	if err = state.pool.QueryRow(context.Background(), "SELECT (value->>'revoked')::boolean FROM agw_mcp_bindings").Scan(&revoked); err != nil || !revoked {
		t.Fatalf("failed cleanup did not retain revoked tombstone: %v", err)
	}
	w = call(b, "PUT", "/v2/service_instances/one/service_bindings/binding-one", bind)
	if w.Code != 409 {
		t.Fatalf("revoked binding resurrected: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	failDelete = false
	mu.Unlock()
	w = call(b, "DELETE", "/v2/service_instances/one/service_bindings/binding-one?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 {
		t.Fatalf("cleanup retry: %s", w.Body)
	}
	w = call(b, "DELETE", "/v2/service_instances/one?service_id=service&plan_id=work-plan", "")
	if w.Code != 200 {
		t.Fatalf("deprovision: %s", w.Body)
	}
}

func TestMCPCatalogAndInstanceSelection(t *testing.T) {
	b, _ := fixture(t)
	// Configure through JSON so this contract fails at runtime before the new
	// configuration types exist, rather than relying on a compilation failure.
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
		t.Fatalf("expected inference and configured MCP plan: %s", w.Body.String())
	}
}

func TestMCPRejectsUnknownOrUnsafeAuthConfiguration(t *testing.T) {
	for _, auth := range []string{
		`{"method":"invented"}`,
		`{"method":"federated_client_credentials","issuer":"https://uaa.test","jwks_url":"https://uaa.test/keys","audience":"urn:exchange","token_endpoint":"https://{tenant}/token","parameters":{"tenant":".*"},"scopes":["read"]}`,
	} {
		b, _ := fixture(t)
		if err := json.Unmarshal([]byte(`{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":`+auth+`}]}`), &b.cfg); err != nil {
			t.Fatal(err)
		}
		if err := b.cfg.validate(); err == nil {
			t.Fatalf("unsafe auth accepted: %s", auth)
		}
	}
}
