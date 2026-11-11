package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func configureInferencePlans(t *testing.T, b *Broker, plans string) {
	t.Helper()
	if err := json.Unmarshal([]byte(`{"inference_plans":`+plans+`}`), &b.cfg); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorConfiguredInferenceCatalogAndLifecycle(t *testing.T) {
	b, resources := fixture(t)
	configureInferencePlans(t, b, `[
		{"id":"starter-id","name":"starter","description":"Team starter allowance","free":true,"models":["small"],"budget":2,"window":"24h"},
		{"id":"large-id","name":"production","description":"Production allowance","free":false,"models":["large"],"budget":25,"window":"1h"}
	]`)
	if err := b.cfg.validate(); err != nil {
		t.Fatal(err)
	}
	w := call(b, "GET", "/v2/catalog", "")
	var catalog struct {
		Services []struct {
			Plans []struct {
				ID, Name, Description string
				Free                  bool
			} `json:"plans"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Services) != 1 || len(catalog.Services[0].Plans) != 2 {
		t.Fatalf("unexpected catalog: %s", w.Body)
	}
	for i, want := range []struct {
		id, name, description string
		free                  bool
	}{{"starter-id", "starter", "Team starter allowance", true}, {"large-id", "production", "Production allowance", false}} {
		got := catalog.Services[0].Plans[i]
		if got.ID != want.id || got.Name != want.name || got.Description != want.description || got.Free != want.free {
			t.Fatalf("operator plan not preserved: %+v", got)
		}
	}
	for _, plan := range []struct{ id, model, amount, window string }{{"starter-id", "small", "2", "24h"}, {"large-id", "large", "25", "1h"}} {
		body := `{"service_id":"service","plan_id":"` + plan.id + `"}`
		w := call(b, "PUT", "/v2/service_instances/"+plan.id, body)
		if w.Code != 201 {
			t.Fatalf("provision %s: %d %s", plan.id, w.Code, w.Body)
		}
		found := false
		for _, r := range *resources {
			if r.Value.Metadata[labelPrefix+"instance"] == plan.id {
				found = true
				if len(r.Value.AllowedModels) != 1 || r.Value.AllowedModels[0] != plan.model || len(r.Value.Budgets) != 1 || r.Value.Budgets[0].Limit.Amount.String() != plan.amount || r.Value.Budgets[0].Window["rolling"] != plan.window {
					t.Fatalf("wrong plan policy: %+v", r.Value)
				}
			}
		}
		if !found {
			t.Fatal("instance resource missing")
		}
		if w := call(b, "PUT", "/v2/service_instances/"+plan.id+"/service_bindings/app", body); w.Code != 201 {
			t.Fatalf("bind: %d %s", w.Code, w.Body)
		}
		if w := call(b, "DELETE", "/v2/service_instances/"+plan.id+"?service_id=service&plan_id="+plan.id, ""); w.Code != 200 {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
	}
}

func TestCatalogValidatesAllPlanIDsAndNamesTogether(t *testing.T) {
	for _, test := range []struct {
		name, inference, mcp string
		valid                bool
	}{
		{"inference duplicate names", `[{"id":"a","name":"team"},{"id":"b","name":"team"}]`, `[]`, false},
		{"inference duplicate IDs", `[{"id":"same","name":"a"},{"id":"same","name":"b"}]`, `[]`, false},
		{"cross-protocol duplicate names", `[{"id":"a","name":"tools"}]`, `[{"name":"tools","plan_id":"b"}]`, false},
		{"cross-protocol duplicate IDs", `[{"id":"same","name":"inference"}]`, `[{"name":"tools","plan_id":"same"}]`, false},
		{"shared is not reserved", `[{"id":"a","name":"inference"}]`, `[{"name":"shared","plan_id":"b"}]`, true},
		{"legacy targets do not publish plans", `[{"id":"a","name":"shared"}]`, `[{"name":"shared"}]`, true},
		{"empty name", `[{"id":"a","name":""}]`, `[]`, false},
		{"whitespace description", `[{"id":"a","name":"inference","description":"  "}]`, `[]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := fixture(t)
			var plans []map[string]any
			if err := json.Unmarshal([]byte(test.inference), &plans); err != nil {
				t.Fatal(err)
			}
			for _, p := range plans {
				if _, ok := p["description"]; !ok {
					p["description"] = "Configured inference"
				}
				p["models"] = []string{"model"}
				p["budget"] = 1
				p["window"] = "24h"
			}
			var backends []map[string]any
			if err := json.Unmarshal([]byte(test.mcp), &backends); err != nil {
				t.Fatal(err)
			}
			for _, p := range backends {
				p["url"] = "https://tools.test/mcp"
				if _, ok := p["plan_id"]; ok {
					p["auth"] = map[string]string{"method": "none", "issuer": "https://uaa.test", "jwks_url": "https://uaa.test/keys", "audience": "urn:test"}
				}
			}
			data, _ := json.Marshal(map[string]any{"inference_plans": plans, "mcp_servers": backends, "mcp_url": "https://gateway.test/mcp"})
			if err := json.Unmarshal(data, &b.cfg); err != nil {
				t.Fatal(err)
			}
			err := b.cfg.validate()
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, got %v", test.valid, err)
			}
			if !test.valid && err != nil && strings.Contains(test.name, "duplicate") && !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("expected duplicate diagnostic, got %v", err)
			}
		})
	}
}
