package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSharedMCPRejectsCollidingTargetsAcrossBackends(t *testing.T) {
	b, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resources := []any{}
		for _, pair := range [][2]string{{"a-b", "c"}, {"a", "b-c"}} {
			resources = append(resources, map[string]any{"value": map[string]any{
				"name": pair[0], "revision": "v1",
				"callerAuth": map[string]any{"issuer": "https://uaa.test", "audiences": []string{"exchange"}, "jwks": map[string]string{"url": "https://uaa.test/keys"}},
				"targets": []any{map[string]any{"name": pair[1], "mcp": map[string]string{"host": "https://tools.test"}}},
			}})
		}
		write(w, 200, map[string]any{"resources": resources})
	}))
	defer server.Close()
	b.cfg.ManagementURL = server.URL
	var plan OfferingPlan
	plan.Access.Backends = []string{"a-b", "a"}
	if _, _, err := b.resolveShared(context.Background(), &plan, nil); err == nil {
		t.Fatal("ambiguous target names must be rejected before provisioning/grant publication")
	}
}

func TestSharedMCPInstanceIdentityRecoveryAndOrphanCleanup(t *testing.T) {
	b, _ := fixture(t)
	raw := `{"service_id":"","inference_plans":[],"services":[{"id":"tools","name":"tools","description":"Tools","protocol":"mcp","connection":{"url":"https://offering.test/mcp"},"plans":[{"id":"work","name":"standard","description":"Work","access":{"backends":["devops"]},"schemas":{"service_instance":{"create":{"parameters":{"type":"object","additionalProperties":false,"required":["client_id"],"properties":{"client_id":{"type":"string","pattern":"^[a-z-]+$"}}}}}}}]}]}`
	if err := json.Unmarshal([]byte(raw), &b.cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	resources := map[string]map[string]json.RawMessage{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/config/resources/"), "/")
		kind := parts[0]
		if kind == "mcp.backend" {
			write(w, 200, map[string]any{"resources": []any{map[string]any{"id": "devops", "value": map[string]any{"name": "devops", "revision": "v1", "callerAuth": map[string]any{"issuer": "https://uaa.test", "audiences": []string{"exchange"}, "jwks": map[string]string{"url": "https://uaa.test/keys"}}, "targets": []any{map[string]any{"name": "work", "mcp": map[string]string{"host": "https://tools.test"}, "policies": map[string]any{"backendAuth": map[string]any{"oauthTokenExchange": map[string]any{"clientAuth": map[string]any{"clientId": map[string]string{"fromInstanceParameter": "client_id"}}}}, "requestHeaderModifier": map[string]any{"set": map[string]string{"X-Key": "shared-secret"}}}}}}}}})
			return
		}
		if resources[kind] == nil {
			resources[kind] = map[string]json.RawMessage{}
		}
		switch r.Method {
		case "GET":
			list := []any{}
			for id, v := range resources[kind] {
				list = append(list, map[string]any{"id": id, "value": v})
			}
			write(w, 200, map[string]any{"resources": list})
		case "PUT":
			var body struct {
				Resources []struct{ Value json.RawMessage }
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(400)
				return
			}
			for _, v := range body.Resources {
				var value struct{ Name string }
				_ = json.Unmarshal(v.Value, &value)
				resources[kind][value.Name] = v.Value
			}
			write(w, 200, map[string]any{})
		case "DELETE":
			delete(resources[kind], parts[1])
			write(w, 200, map[string]any{})
		}
	}))
	defer server.Close()
	b.cfg.ManagementURL = server.URL
	if err := b.cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if w := call(b, "PUT", "/v2/service_instances/one", `{"service_id":"tools","plan_id":"work"}`); w.Code != 400 {
		t.Fatalf("missing identity: %d %s", w.Code, w.Body)
	}
	provision := `{"service_id":"tools","plan_id":"work","parameters":{"client_id":"selected-principal"}}`
	if w := call(b, "PUT", "/v2/service_instances/one", provision); w.Code != 201 {
		t.Fatalf("provision: %d %s", w.Code, w.Body)
	}
	replica, err := newBroker(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := call(replica, "PUT", "/v2/service_instances/one", provision); w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	bind := `{"service_id":"tools","plan_id":"work","bind_resource":{"app_guid":"app"},"context":{"platform":"cloudfoundry","service_account":{"issuer":"https://uaa.test","subject":"worker"}}}`
	w := call(replica, "PUT", "/v2/service_instances/one/service_bindings/app", bind)
	if w.Code != 201 || !strings.Contains(w.Body.String(), "https://offering.test/mcp/one/app") {
		t.Fatalf("bind: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	data, _ := json.Marshal(resources)
	mu.Unlock()
	if strings.Contains(string(data), "shared-secret") || !strings.Contains(string(data), "backendSelections") || !strings.Contains(string(data), "selected-principal") {
		t.Fatalf("grant must contain references and selection, no secrets: %s", data)
	}
	for _, binding := range []string{"app", "orphan-never-returned"} {
		w := call(replica, "DELETE", "/v2/service_instances/one/service_bindings/"+binding+"?service_id=tools&plan_id=work", "")
		if w.Code != 200 {
			t.Fatalf("orphan cleanup: %d %s", w.Code, w.Body)
		}
	}
	if w := call(replica, "DELETE", "/v2/service_instances/one?service_id=tools&plan_id=work", ""); w.Code != 200 {
		t.Fatalf("deprovision: %d %s", w.Code, w.Body)
	}
}
