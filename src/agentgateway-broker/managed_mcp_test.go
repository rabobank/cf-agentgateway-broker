package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMCPTranslatorRecoversFromGatewayStateWithoutDatabase(t *testing.T) {
	b, _ := fixture(t)
	config := `{"mcp_url":"https://gateway.test/mcp","mcp_servers":[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":{"method":"passthrough","issuer":"https://uaa.test/oauth/token","jwks_url":"https://uaa.test/token_keys","audience":"urn:test:exchange"}}]}`
	if err := json.Unmarshal([]byte(config), &b.cfg); err != nil { t.Fatal(err) }
	var mu sync.Mutex
	resources := map[string]map[string]json.RawMessage{}
	deleted := map[string]bool{}
	loseResponse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock(); defer mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path,"/api/config/resources/"),"/")
		kind := parts[0]
		if resources[kind] == nil { resources[kind] = map[string]json.RawMessage{} }
		if r.Method == http.MethodGet {
			var list []any
			for id, value := range resources[kind] { list = append(list,map[string]any{"id":id,"value":value}) }
			write(w,200,map[string]any{"resources":list}); return
		}
		if r.Method == http.MethodDelete {
			if len(parts)!=2 {w.WriteHeader(400);return}
			delete(resources[kind],parts[1]);deleted[kind+"/"+parts[1]]=true;write(w,200,map[string]any{});return
		}
		var body struct {Resources []struct{Value json.RawMessage `json:"value"`} `json:"resources"`}
		if json.NewDecoder(r.Body).Decode(&body)!=nil {w.WriteHeader(400);return}
		for _,v:=range body.Resources {
			var value struct{Name string `json:"name"`};if json.Unmarshal(v.Value,&value)!=nil||value.Name==""{w.WriteHeader(400);return}
			if deleted[kind+"/"+value.Name] {w.WriteHeader(409);return}
			if old,ok:=resources[kind][value.Name];ok&&string(old)!=string(v.Value){w.WriteHeader(409);return}
			resources[kind][value.Name]=v.Value
		}
		if loseResponse {loseResponse=false;c,_,_:=w.(http.Hijacker).Hijack();c.Close();return}
		write(w,200,map[string]any{})
	}));defer server.Close();b.cfg.ManagementURL=server.URL
	provision:=`{"service_id":"service","plan_id":"work-plan"}`
	w:=call(b,"PUT","/v2/service_instances/one",provision);if w.Code!=201{t.Fatalf("provision: %d %s",w.Code,w.Body)}
	mu.Lock();if len(resources["mcp.instance"])!=1{t.Error("instance selection was not persisted through gateway management")};mu.Unlock()
	bind:=`{"service_id":"service","plan_id":"work-plan","bind_resource":{"app_guid":"app"},"context":{"platform":"cloudfoundry","service_account":{"issuer":"https://uaa.test/oauth/token","subject":"cf:service-account:team:worker"}}}`
	path:="/v2/service_instances/one/service_bindings/binding-one"
	mu.Lock();loseResponse=true;mu.Unlock()
	w=call(b,"PUT",path,bind);if w.Code!=503{t.Fatalf("expected uncertain grant write: %d %s",w.Code,w.Body)}
	// Another broker replica has only operator configuration and API credentials.
	other,err:=newBroker(b.cfg);if err!=nil{t.Fatal(err)}
	w=call(other,"PUT",path,bind);if w.Code!=200&&w.Code!=201{t.Fatalf("restart retry: %d %s",w.Code,w.Body)}
	mu.Lock();if len(resources["mcp.grant"])!=1{t.Errorf("retry produced %d grants",len(resources["mcp.grant"]))};mu.Unlock()
	if !strings.Contains(w.Body.String(),"/mcp/one/binding-one")||strings.Contains(w.Body.String(),"api_key"){t.Fatalf("credentials: %s",w.Body)}
	w=call(other,"DELETE",path+"?service_id=service&plan_id=work-plan","");if w.Code!=200&&w.Code!=410{t.Fatalf("unbind: %d %s",w.Code,w.Body)}
	mu.Lock();if len(resources["mcp.grant"])!=0{t.Error("unbind left gateway grant")};mu.Unlock()
	// CAPI can orphan-unbind a bind whose response was never received.
	mu.Lock();loseResponse=true;mu.Unlock()
	orphan:="/v2/service_instances/one/service_bindings/orphan"
	w=call(b,"PUT",orphan,bind);if w.Code!=503{t.Fatalf("orphan setup: %d %s",w.Code,w.Body)}
	w=call(other,"DELETE",orphan+"?service_id=service&plan_id=work-plan","");if w.Code!=200&&w.Code!=410{t.Fatalf("orphan cleanup: %d %s",w.Code,w.Body)}
	w=call(other,"DELETE","/v2/service_instances/one?service_id=service&plan_id=work-plan","");if w.Code!=200{t.Fatalf("deprovision: %d %s",w.Code,w.Body)}
	mu.Lock();defer mu.Unlock()
	if len(resources["mcp.instance"])!=0||len(resources["mcp.grant"])!=0{t.Error("deprovision left lifecycle resources")}
}
