package main

import "testing"

func TestDatabaseBinding(t *testing.T) {
	raw := `{"PostgreSQL":[{"name":"agentgateway-state","credentials":{"uri":"postgres://user:password@database.apps.internal:5432/postgres?sslmode=disable"}}]}`
	uri, err := database(raw, "agentgateway-state")
	if err != nil || uri == "" {
		t.Fatal("binding was not resolved")
	}
	for _, bad := range []string{`{}`, `{"PostgreSQL":[{"name":"other","credentials":{"uri":"postgres://db/x"}}]}`, `{"PostgreSQL":[{"name":"agentgateway-state","credentials":{"uri":"https://db/x"}}]}`} {
		if _, err = database(bad, "agentgateway-state"); err == nil {
			t.Fatal("invalid binding accepted")
		}
	}
	if _, err = database(raw, "custom-state"); err == nil {
		t.Fatal("resolved a binding with a different name")
	}
	custom := `{"PostgreSQL":[{"name":"custom-state","credentials":{"url":"postgresql://db/postgres"}}]}`
	if uri, err = database(custom, "custom-state"); err != nil || uri != "postgresql://db/postgres" {
		t.Fatalf("custom binding was not resolved: %q, %v", uri, err)
	}
}
