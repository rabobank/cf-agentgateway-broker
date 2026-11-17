package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorGatewayAssemblyKeepsProvidersAndHidesWiring(t *testing.T) {
	t.Setenv("VCAP_SERVICES",`{"postgres":[{"name":"state","credentials":{"uri":"postgres://db/gateway"}}]}`)
	raw:=`{"database":{"service_name":"state"},"llm":{"providers":[{"name":"azure","provider":"azure","params":{"apiKey":"private-key","azureResourceName":"resource"}}],"models":[{"name":"chat","provider":{"reference":"azure"}}]},"model_catalog":{"providers":{}},"mcp":{"session_key":"`+strings.Repeat("12",32)+`","caller_auth":{"issuer":"https://uaa.test","audiences":["exchange"],"jwks":{"url":"https://uaa.test/keys"}},"backends":[{"name":"tools","revision":"v1","mcp":{"targets":[{"name":"work","mcp":{"host":"https://tools.test/mcp"},"policies":{"backendAuth":{"oauthTokenExchange":{"clientAuth":{"clientId":{"fromInstanceParameter":"client_id"}}}}}}]}}]},"capsules":{"log_id":"lab","signing_seed":"`+strings.Repeat("34",32)+`"}}`
	cfg,err:=operatorGateway([]byte(raw),8123);if err!=nil {t.Fatal(err)}
	data,_:=json.Marshal(cfg)
	var got map[string]any;if err:=json.Unmarshal(data,&got);err!=nil {t.Fatal(err)}
	if got["gateways"].(map[string]any)["inference"].(map[string]any)["port"]!=float64(8123) {t.Fatal("CF port not injected")}
	if !strings.Contains(string(data),`"reference":"azure"`)||!strings.Contains(string(data),`"apiKey":"private-key"`) {t.Fatal("native configuration lost")}
	if len(got["mcp"].(map[string]any)["targets"].([]any))!=0 {t.Fatal("operator backend publicly exposed")}
	if len(got["mcpBackends"].([]any))!=1 {t.Fatal("shared configuration unavailable to gateway")}
	if strings.Contains(string(data),"34343434") {t.Fatal("capsule signing key forwarded to native config")}
}
