package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfiguredMCPPlansAreNotGloballyExposed(t *testing.T) {
	config, err := mcpConfig(`[{"name":"work","plan_id":"work-plan","url":"https://tools.test/mcp","auth":{"method":"passthrough","issuer":"https://uaa.test/oauth/token","jwks_url":"https://uaa.test/token_keys","audience":"urn:exchange"}}]`)
	if err != nil { t.Fatal(err) }
	data, err := json.Marshal(config)
	if err != nil { t.Fatal(err) }
	if strings.Contains(string(data), "https://tools.test/mcp") { t.Fatalf("plan backend exposed globally: %s",data) }
}
