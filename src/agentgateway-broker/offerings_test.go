package main

import (
	"encoding/json"
	"testing"
)

func TestIndependentOfferingsAndOwnership(t *testing.T) {
	b, _ := fixture(t)
	data := `{"service_id":"","inference_plans":[],"services":[
	 {"id":"teams","name":"teams","description":"Team inference","protocol":"llm","connection":{"url":"https://teams.test/v1"},"plans":[{"id":"team-small","name":"small","description":"Team small","free":true,"access":{"models":["small"]},"budget":{"usd":10,"window":"24h"}}]},
	 {"id":"prod","name":"prod","description":"Production inference","protocol":"llm","connection":{"url":"https://prod.test/v1"},"plans":[{"id":"prod-small","name":"small","description":"Production small","access":{"models":["large"]},"budget":{"usd":50,"window":"24h"}}]}
	]}`
	if err := json.Unmarshal([]byte(data), &b.cfg); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.validate(); err != nil {
		t.Fatalf("independent offerings: %v", err)
	}
	w := call(b, "GET", "/v2/catalog", "")
	var catalog struct {
		Services []struct {
			ID    string
			Plans []struct{ Name string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Services) != 2 || catalog.Services[0].ID != "teams" || catalog.Services[1].Plans[0].Name != "small" {
		t.Fatalf("catalog: %s", w.Body)
	}
	body := `{"service_id":"teams","plan_id":"team-small"}`
	if w := call(b, "PUT", "/v2/service_instances/team", body); w.Code != 201 {
		t.Fatalf("provision: %d %s", w.Code, w.Body)
	}
	// Another replica recovers policy and credentials entirely from gateway state.
	replica, err := newBroker(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	w = call(replica, "PUT", "/v2/service_instances/team/service_bindings/app", body)
	var binding struct{ Credentials map[string]string }
	if err := json.Unmarshal(w.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	if w.Code != 201 || binding.Credentials["url"] != "https://teams.test/v1" {
		t.Fatalf("offering connection: %d %s", w.Code, w.Body)
	}
	if w := call(replica, "PUT", "/v2/service_instances/team/service_bindings/wrong", `{"service_id":"prod","plan_id":"team-small"}`); w.Code != 400 {
		t.Fatalf("cross-offering plan: %d %s", w.Code, w.Body)
	}
	if w := call(replica, "DELETE", "/v2/service_instances/team?service_id=prod&plan_id=prod-small", ""); w.Code != 409 {
		t.Fatalf("cross-offering deletion: %d %s", w.Code, w.Body)
	}
}

func TestOfferingRejectsNonfiniteBudgetAndDuplicateAccess(t *testing.T) {
	for _, budget := range []string{`1e999`, `10`} {
		b,_:=fixture(t)
		raw:=`{"service_id":"","inference_plans":[],"services":[{"id":"team","name":"team","description":"Team","protocol":"llm","connection":{"url":"https://gateway.test/v1"},"plans":[{"id":"small","name":"small","description":"Small","access":{"models":["small","small"]},"budget":{"usd":`+budget+`,"window":"24h"}}]}]}`
		if err:=json.Unmarshal([]byte(raw),&b.cfg);err!=nil {t.Fatal(err)}
		if err:=b.cfg.validate();err==nil {t.Fatal("invalid or duplicate access accepted")}
	}
}
