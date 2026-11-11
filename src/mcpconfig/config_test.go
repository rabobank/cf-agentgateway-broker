package mcpconfig

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestUpstreamHeaderSyntax(t *testing.T) {
	for _, test := range []struct {
		name, key, value string
		valid            bool
	}{
		{"space in name", "X Backend Key", "value", false},
		{"colon in name", "X:Key", "value", false},
		{"tab in name", "X\tKey", "value", false},
		{"non-ASCII name", "X-é", "value", false},
		{"empty name", "", "value", false},
		{"NUL value", "X-Key", "a\x00b", false},
		{"newline value", "X-Key", "a\nb", false},
		{"token punctuation", "!#$%&'*+-.^_`|~012AZaz", "value", true},
		{"tabs and non-ASCII value", "X-Key", "\t café \t", true},
		{"empty value", "X-Key", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := Backend{Name: "tools", URL: "https://tools.example/mcp", Headers: map[string]string{test.key: test.value}}
			err := Validate([]Backend{backend})
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, got %v", test.valid, err)
			}
		})
	}
	for c := byte(0); c < 128; c++ {
		if c >= 32 && c != 127 || c == '\t' {
			continue
		}
		t.Run(fmt.Sprintf("value-control-%02x", c), func(t *testing.T) {
			backend := Backend{Name: "tools", URL: "https://tools.example/mcp", Headers: map[string]string{"X-Key": string([]byte{'a', c, 'b'})}}
			if err := Validate([]Backend{backend}); err == nil {
				t.Fatalf("control byte %02x accepted", c)
			}
		})
	}
}

func TestRouteResourceIsStableButBackendTargetChangesWithGeneration(t *testing.T) {
	binding := Binding{ID: "binding", Generation: "first", Instance: Instance{ID: "instance", Installation: "installation", Generation: "instance-first", Backend: Backend{Name: "tools", Auth: Auth{Method: "none"}}}}
	inspect := func(b Binding) (string, string) {
		t.Helper()
		data, _ := json.Marshal(b.Route())
		var route struct {
			Name     string `json:"name"`
			Backends []struct {
				MCP struct {
					Targets []struct {
						Name string `json:"name"`
					} `json:"targets"`
				} `json:"mcp"`
			} `json:"backends"`
		}
		if err := json.Unmarshal(data, &route); err != nil {
			t.Fatal(err)
		}
		return route.Name, route.Backends[0].MCP.Targets[0].Name
	}
	firstRoute, firstTarget := inspect(binding)
	binding.Generation = "second"
	binding.Instance.Generation = "instance-second"
	secondRoute, secondTarget := inspect(binding)
	if firstRoute != secondRoute {
		t.Errorf("route resource changes across retries/recreation: %s vs %s", firstRoute, secondRoute)
	}
	if firstTarget == secondTarget {
		t.Error("backend target must remain generation-specific for session isolation")
	}
	if firstRoute == firstTarget {
		t.Error("route identity must be distinct from target identity")
	}
}

func TestFederationParameterIsolation(t *testing.T) {
	b := Backend{Name: "tools", PlanID: "tools-plan", URL: "https://tools.example/mcp", Auth: Auth{Method: "federated_client_credentials", Issuer: "https://issuer.example/token", JWKSURL: "https://issuer.example/keys", Audience: "urn:exchange", TokenEndpoint: "https://tokens.example/{tenant}/token", Scopes: []string{"read"}, Parameters: map[string]string{"tenant": "[a-z0-9.-]+"}}}
	if err := Validate([]Backend{b}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"../evil", "..", "a/b", "a%2fb", "a?x", "a#x"} {
		if _, err := b.Resolve(map[string]any{"client_id": "client", "tenant": tenant}); err == nil {
			t.Fatalf("unsafe tenant accepted: %s", tenant)
		}
	}
	params, err := b.Resolve(map[string]any{"client_id": "client", "tenant": "team"})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Endpoint(params); got != "https://tokens.example/team/token" {
		t.Fatal(got)
	}
	if _, err = b.Resolve(map[string]any{"client_id": "client", "tenant": "team", "endpoint": "https://evil"}); err == nil {
		t.Fatal("unknown input accepted")
	}
}
func TestSelectedIdentityForEveryAuthMethod(t *testing.T) {
	for _, method := range []string{"none", "static", "passthrough", "federated_client_credentials"} {
		b := Binding{Instance: Instance{Backend: Backend{Name: "tools", Auth: Auth{Method: method, IdentityReference: "operator-selected"}}}}
		identity := b.Selected()
		if identity.BackendID != "tools" || identity.AuthMethod != method || identity.IdentityReference != "operator-selected" {
			t.Fatalf("missing selected identity: %#v", identity)
		}
	}
}
