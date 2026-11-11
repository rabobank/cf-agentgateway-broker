package main

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T) (*Broker, *map[string]Resource) {
	t.Helper()
	resources := map[string]Resource{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const prefix = "/api/config/resources/llm.apiKey"
		switch r.Method {
		case http.MethodGet:
			list := []Resource{}
			for _, item := range resources {
				list = append(list, item)
			}
			write(w, 200, map[string]any{"resources": list})
		case http.MethodPut:
			var payload struct {
				Resources []Resource `json:"resources"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			for _, item := range payload.Resources {
				item.ID = fmt.Sprintf("key-%d", len(resources)+1)
				resources[item.ID] = item
			}
			write(w, 200, map[string]any{})
		case http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, prefix+"/")
			if _, ok := resources[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(resources, id)
			write(w, 200, map[string]any{})
		}
	}))
	t.Cleanup(server.Close)
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certServer.Close)
	caFile := filepath.Join(t.TempDir(), "inference-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := newBroker(Config{Listen: ":8080", Username: "broker", Password: "secret", Installation: "test", GatewayURL: "https://gateway.test/v1", ManagementURL: server.URL, TrustedCAFile: caFile, ServiceID: "service", InferencePlans: []InferencePlan{{ID: "plan", Name: "inference", Description: "Test inference", Free: true, Models: []string{"model"}, Budget: json.Number("1.25"), Window: "24h"}}})
	if err != nil {
		t.Fatal(err)
	}
	return b, &resources
}
func call(b *Broker, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetBasicAuth("broker", "secret")
	r.Header.Set("X-Broker-API-Version", "2.16")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	return w
}

const provision = `{"service_id":"service","plan_id":"plan","organization_guid":"org","space_guid":"space"}`
const binding = `{"service_id":"service","plan_id":"plan"}`

func TestLifecycleAndRestartRecovery(t *testing.T) {
	b, resources := fixture(t)
	if w := call(b, "PUT", "/v2/service_instances/instance", provision); w.Code != 201 {
		t.Fatalf("provision: %d %s", w.Code, w.Body)
	}
	if w := call(b, "PUT", "/v2/service_instances/instance", provision); w.Code != 200 {
		t.Fatalf("retry: %d", w.Code)
	}
	if len(*resources) != 1 {
		t.Fatal("retry created duplicate")
	}
	// A new broker has no local state and still returns the same shared credentials.
	restarted, err := newBroker(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	first := call(b, "PUT", "/v2/service_instances/instance/service_bindings/app", binding)
	second := call(restarted, "PUT", "/v2/service_instances/instance/service_bindings/key", binding)
	if first.Code != 201 || first.Body.String() != second.Body.String() {
		t.Fatalf("credentials not stable: %s %s", first.Body, second.Body)
	}
	if !strings.Contains(first.Body.String(), "sk-cf-") {
		t.Fatal("credential missing")
	}
	var response struct {
		Credentials map[string]string `json:"credentials"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	expectedCA, err := os.ReadFile(b.cfg.TrustedCAFile)
	if err != nil {
		t.Fatal(err)
	}
	if response.Credentials["trusted_ca"] != string(expectedCA) {
		t.Fatal("binding must include the complete inference CA PEM")
	}
	if w := call(b, "DELETE", "/v2/service_instances/instance/service_bindings/app?service_id=service&plan_id=plan", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(*resources) != 1 {
		t.Fatal("unbind revoked shared key")
	}
	if w := call(b, "DELETE", "/v2/service_instances/instance?service_id=service&plan_id=plan", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call(b, "DELETE", "/v2/service_instances/instance?service_id=service&plan_id=plan", ""); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if len(*resources) != 0 {
		t.Fatal("deprovision left key")
	}
}

func TestInvalidInferenceCARejected(t *testing.T) {
	b, _ := fixture(t)
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := b.cfg
	cfg.TrustedCAFile = path
	if _, err := newBroker(cfg); err == nil {
		t.Fatal("invalid inference CA accepted")
	}
}
func TestProvisionConflictAndOwnership(t *testing.T) {
	b, resources := fixture(t)
	call(b, "PUT", "/v2/service_instances/instance", provision)
	if w := call(b, "PUT", "/v2/service_instances/instance", strings.Replace(provision, `"org"`, `"other"`, 1)); w.Code != 409 {
		t.Fatal(w.Code)
	}
	var owned Resource
	for _, r := range *resources {
		owned = r
	}
	foreign := owned
	foreign.ID = "foreign"
	foreign.Value.Metadata = map[string]any{labelPrefix + "instance": "instance", labelPrefix + "installation": "another"}
	(*resources)[foreign.ID] = foreign
	if w := call(b, "PUT", "/v2/service_instances/instance", provision); w.Code != 200 {
		t.Fatal(w.Code)
	}
	duplicate := owned
	duplicate.ID = "duplicate"
	(*resources)[duplicate.ID] = duplicate
	if w := call(b, "PUT", "/v2/service_instances/instance", provision); w.Code != 503 {
		t.Fatal("duplicate was accepted", w.Code)
	}
}

func TestEmptyParametersHaveIdenticalFingerprint(t *testing.T) {
	first := Request{ServiceID: "service", PlanID: "plan"}
	second := first
	second.Parameters = map[string]any{}
	if fingerprint(first) != fingerprint(second) {
		t.Fatal("empty and omitted parameters must be equivalent")
	}
}

func TestNewerOSBMinorVersion(t *testing.T) {
	b, _ := fixture(t)
	r := httptest.NewRequest("GET", "/v2/catalog", nil)
	r.SetBasicAuth("broker", "secret")
	r.Header.Set("X-Broker-API-Version", "2.15")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestAuthenticationValidationAndUnavailableGateway(t *testing.T) {
	b, _ := fixture(t)
	r := httptest.NewRequest("GET", "/v2/catalog", nil)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r.SetBasicAuth("broker", "secret")
	w = httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != 412 {
		t.Fatal(w.Code)
	}
	if w := call(b, "PUT", "/v2/service_instances/instance", `{"service_id":"wrong","plan_id":"plan"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call(b, "PUT", "/v2/service_instances/instance", provision+`{}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call(b, "PUT", "/v2/service_instances/instance", `{"service_id":"service","plan_id":"plan","parameters":{"model":"other"}}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	b.cfg.ManagementURL = "http://127.0.0.1:1"
	if w := call(b, "PUT", "/v2/service_instances/instance", provision); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
