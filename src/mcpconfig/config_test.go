package mcpconfig

import "testing"

func TestFederationParameterIsolation(t *testing.T) {
	b := Backend{Name: "tools", PlanID: "tools-plan", URL: "https://tools.example/mcp", Auth: Auth{Method: "federated_client_credentials", Issuer: "https://issuer.example/token", JWKSURL: "https://issuer.example/keys", Audience: "urn:exchange", TokenEndpoint: "https://tokens.example/{tenant}/token", Scopes: []string{"read"}, Parameters: map[string]string{"tenant": "[a-z0-9.-]+"}}}
	if err := Validate([]Backend{b}, "inference"); err != nil {
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
