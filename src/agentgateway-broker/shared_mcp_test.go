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
				"targets":    []any{map[string]any{"name": pair[1], "mcp": map[string]string{"host": "https://tools.test"}}},
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

func TestSharedMCPAttributesLiteralAndInstanceSelectedClientIDs(t *testing.T) {
	for _, test := range []struct {
		name       string
		client     any
		parameters map[string]string
		want       string
	}{
		{"literal", "operator-client", nil, "operator-client"},
		{"instance selected", map[string]string{"fromInstanceParameter": "client_id"}, map[string]string{"client_id": "developer-client"}, "developer-client"},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := fixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				write(w, 200, map[string]any{"resources": []any{map[string]any{"value": map[string]any{
					"name": "tools", "revision": "v1", "callerAuth": map[string]any{"issuer": "https://uaa.test", "audiences": []string{"exchange"}, "jwks": map[string]string{"url": "https://uaa.test/keys"}},
					"targets": []any{map[string]any{"name": "work", "policies": map[string]any{"backendAuth": map[string]any{"oauthTokenExchange": map[string]any{"host": "https://tokens.test", "clientAuth": map[string]any{"clientId": test.client}}}}}},
				}}}})
			}))
			defer server.Close()
			b.cfg.ManagementURL = server.URL
			var plan OfferingPlan
			plan.Access.Backends = []string{"tools"}
			_, selections, err := b.resolveShared(context.Background(), &plan, test.parameters)
			if err != nil {
				t.Fatal(err)
			}
			identity := selections[0].Identities["work"]
			if identity.ClientID != test.want || identity.Authority != "https://tokens.test" {
				t.Fatalf("known upstream identity lost: %+v", identity)
			}
		})
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
				if len(parts) == 2 && parts[1] != id {
					continue
				}
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

func TestTeamOAuthRejectsBindingKeyWithInvalidSuffix(t *testing.T) {
	b, _ := fixture(t)
	key := strings.Repeat("12", 32)
	for _, suffix := range []string{"a", "zz"} {
		t.Run(suffix, func(t *testing.T) {
			cfg := b.cfg
			cfg.TeamOAuth = &teamOAuthBroker{BindingKey: key + suffix}
			if _, err := newBroker(cfg); err == nil || !strings.Contains(err.Error(), "32-byte hexadecimal binding key") {
				t.Fatalf("expected malformed binding key to prevent startup; got %v", err)
			}
		})
	}
}

func TestTeamOAuthBindingRecoverableAndUnique(t *testing.T) {
	b, _ := fixture(t)
	raw := `{"service_id":"","inference_plans":[],"team_oauth":{"binding_key":"` + strings.Repeat("12", 32) + `"},"services":[{"id":"tools","name":"tools","description":"Tools","protocol":"mcp","connection":{"url":"https://gateway.test/mcp"},"plans":[{"id":"reader","name":"reader","description":"Read","access":{"backends":["design"]}}]}]}`
	if err := json.Unmarshal([]byte(raw), &b.cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	resources := map[string]map[string]json.RawMessage{}
	revoked := map[string]bool{}
	lost := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		kind, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/config/resources/"), "/")
		if kind == "mcp.backend" {
			w.Write([]byte(`{"resources":[{"id":"design","value":{"name":"design","revision":"v1","teamOAuth":true,"targets":[{"name":"reader"}]}}]}`))
			return
		}
		if resources[kind] == nil {
			resources[kind] = map[string]json.RawMessage{}
		}
		switch r.Method {
		case "GET":
			var list []any
			for resourceID, value := range resources[kind] {
				if id == "" || resourceID == id {
					list = append(list, map[string]any{"id": resourceID, "value": value})
				}
			}
			write(w, 200, map[string]any{"resources": list})
		case "PUT":
			var body struct {
				Resources []struct{ Value json.RawMessage }
			}
			var value struct{ Name string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Resources) != 1 || json.Unmarshal(body.Resources[0].Value, &value) != nil {
				w.WriteHeader(400)
				return
			}
			if revoked[value.Name] {
				w.WriteHeader(409)
				return
			}
			resources[kind][value.Name] = body.Resources[0].Value
			if lost && kind == "mcp.grant" {
				lost = false
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			write(w, 200, map[string]any{})
		case "DELETE":
			revoked[id] = true
			delete(resources[kind], id)
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	b.cfg.ManagementURL = s.URL
	const path = "/v2/service_instances/team"
	const provision = `{"service_id":"tools","plan_id":"reader"}`
	first := call(b, "PUT", path, provision)
	if first.Code != 201 {
		t.Fatalf("provision: %d %s", first.Code, first.Body)
	}
	other, err := newBroker(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again := call(other, "PUT", path, provision); again.Code != 200 || again.Body.String() != first.Body.String() {
		t.Fatal("provision retry did not return the same response")
	}
	const bind = `{"service_id":"tools","plan_id":"reader","bind_resource":{"app_guid":"agent"},"context":{"platform":"cloudfoundry"}}`
	const bindingPath = path + "/service_bindings/"
	mu.Lock()
	lost = true
	mu.Unlock()
	if w := call(b, "PUT", bindingPath+"first", bind); w.Code != 503 {
		t.Fatal("expected uncertain write")
	}
	credential := func(b *Broker, id string) string {
		t.Helper()
		var result struct {
			Credentials struct{ Headers map[string]string }
		}
		w := call(b, "PUT", bindingPath+id, bind)
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Credentials.Headers["Authorization"]) != 71 {
			t.Fatalf("missing binding credential, HTTP %d", w.Code)
		}
		return result.Credentials.Headers["Authorization"]
	}
	firstKey := credential(other, "first")
	if credential(b, "first") != firstKey || credential(other, "second") == firstKey {
		t.Fatal("binding credentials are not stable and distinct")
	}
	mu.Lock()
	durable := string(resources["mcp.grant"][managedID(b.cfg.Installation, "team", "first")])
	mu.Unlock()
	if strings.Contains(durable, strings.TrimPrefix(firstKey, "Bearer ")) || !strings.Contains(durable, "bearer_hash") {
		t.Fatal("expected a persisted verifier only")
	}
	for _, id := range []string{"first", "orphan"} {
		if w := call(other, "DELETE", bindingPath+id+"?service_id=tools&plan_id=reader", ""); w.Code != 200 {
			t.Fatal("orphan cleanup failed")
		}
		if w := call(b, "PUT", bindingPath+id, bind); w.Code != 503 && w.Code != 409 {
			t.Fatal("orphan binding resurrected")
		}
	}
}
