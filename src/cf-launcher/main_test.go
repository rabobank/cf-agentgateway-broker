package main

import (
	"strings"
	"testing"
)

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

func TestDatabaseDiagnosticsDoNotExposeCredentials(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`{"PostgreSQL":[{"name":"state","credentials":{"uri":"postgres://user:secret-password@db:bad-port"}}]}`, `binding "state" credentials.uri`},
		{`{"PostgreSQL":[{"name":"state","credentials":{"password":"secret-password"}}]}`, "credentials.uri or credentials.url must be a string"},
		{`{}`, `binding "state" not found`},
		{`{"PostgreSQL":secret-password}`, "VCAP_SERVICES: invalid JSON syntax at byte"},
	} {
		_, err := database(test.raw, "state")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("want %q, got %v", test.want, err)
		}
		if strings.Contains(err.Error(), "secret-password") {
			t.Fatalf("credential leaked: %v", err)
		}
	}
}
