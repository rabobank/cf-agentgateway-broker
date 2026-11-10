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
	var losePutResponses int
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
		if r.Method == "GET" {
			var resources []any
			for name, value := range routes {
				resources = append(resources, map[string]any{"id": name, "value": value})
			}
			write(w, 200, map[string]any{"resources": resources})
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
		if losePutResponses > 0 {
			losePutResponses--
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack management response: %v", err)
				return
			}
			_ = conn.Close()
			return
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
	t.Run("lost route response is repaired by retry without a duplicate route", func(t *testing.T) {
		mu.Lock()
		losePutResponses = 1
		mu.Unlock()
		path := "/v2/service_instances/one/service_bindings/lost-response"
		w := call(b, "PUT", path, bind)
		if w.Code != 503 {
			t.Fatalf("expected uncertain route write: %d %s", w.Code, w.Body)
		}
		var count int
		if err := state.pool.QueryRow(context.Background(), "SELECT count(*) FROM agw_mcp_bindings WHERE id='lost-response'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected grant after lost response: %d %v", count, err)
		}
		mu.Lock()
		var original json.RawMessage
		for _, value := range routes {
			original = append(json.RawMessage(nil), value...)
		}
		mu.Unlock()
		w = call(b, "PUT", path, bind)
		if w.Code != 201 {
			t.Fatalf("retry: %d %s", w.Code, w.Body)
		}
		mu.Lock()
		if len(routes) != 1 {
			t.Errorf("retry left %d competing routes for one binding URL", len(routes))
		}
		for _, value := range routes {
			if string(value) == string(original) {
				t.Error("retry reused the uncommitted target generation")
			}
		}
		mu.Unlock()
		w = call(b, "DELETE", path+"?service_id=service&plan_id=work-plan", "")
		if w.Code != 200 {
			t.Fatalf("cleanup after retry: %d %s", w.Code, w.Body)
		}
		mu.Lock()
		if len(routes) != 0 {
			t.Errorf("cleanup left %d routes", len(routes))
		}
		clear(routes)
		mu.Unlock()
	})
	t.Run("orphan mitigation deletes a route without a committed grant", func(t *testing.T) {
		mu.Lock()
		losePutResponses = 1
		mu.Unlock()
		path := "/v2/service_instances/one/service_bindings/orphan"
		w := call(b, "PUT", path, bind)
		if w.Code != 503 {
			t.Fatalf("expected lost response: %d %s", w.Code, w.Body)
		}
		w = call(b, "DELETE", path+"?service_id=service&plan_id=work-plan", "")
		if w.Code != 200 && w.Code != 410 {
			t.Fatalf("orphan cleanup: %d %s", w.Code, w.Body)
		}
		mu.Lock()
		if len(routes) != 0 {
			t.Errorf("orphan DELETE left %d routes without grants", len(routes))
		}
		clear(routes)
		mu.Unlock()
	})
	t.Run("grant commit failure is recoverable by a binding retry", func(t *testing.T) {
		ctx := context.Background()
		_, err := state.pool.Exec(ctx, `CREATE FUNCTION reject_test_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$; CREATE CONSTRAINT TRIGGER reject_test_grant AFTER INSERT ON agw_mcp_bindings DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_test_grant()`)
		if err != nil {
			t.Fatal(err)
		}
		path := "/v2/service_instances/one/service_bindings/commit-failure"
		w := call(b, "PUT", path, bind)
		_, err = state.pool.Exec(ctx, `DROP TRIGGER reject_test_grant ON agw_mcp_bindings; DROP FUNCTION reject_test_grant()`)
		if err != nil {
			t.Fatal(err)
		}
		if w.Code != 503 {
			t.Fatalf("expected failed commit: %d %s", w.Code, w.Body)
		}
		w = call(b, "PUT", path, bind)
		if w.Code != 201 {
			t.Fatalf("retry: %d %s", w.Code, w.Body)
		}
		mu.Lock()
		if len(routes) != 1 {
			t.Errorf("retry after commit failure left %d routes", len(routes))
		}
		mu.Unlock()
		w = call(b, "DELETE", path+"?service_id=service&plan_id=work-plan", "")
		if w.Code != 200 {
			t.Fatalf("cleanup: %d %s", w.Code, w.Body)
		}
		mu.Lock()
		if len(routes) != 0 {
			t.Errorf("cleanup left %d routes", len(routes))
		}
		clear(routes)
		mu.Unlock()
	})
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

func TestMCPCatalogPlanDescriptions(t *testing.T) {
	for _, test := range []struct {
		name, description, want string
	}{
		{"omitted", "", "Access to the work MCP backend"},
		{"whitespace", " \t\n", "Access to the work MCP backend"},
		{"custom", "Team-owned read-only tools", "Team-owned read-only tools"},
		{"preserves custom formatting", " Team tools ", " Team tools "},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := fixture(t)
			config := map[string]any{"mcp_url": "https://gateway.test/mcp", "mcp_servers": []any{map[string]any{
				"name": "work", "plan_id": "work-plan", "url": "https://tools.test/mcp",
			}}}
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
