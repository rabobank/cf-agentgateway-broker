package main

import (
	"encoding/json"
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

func TestPostgreSQLURLDiagnosticsExcludeSecrets(t *testing.T) {
	for _, field := range []string{"uri", "url"} {
		for _, invalid := range []struct{ name, value string }{
			{"invalid port", "postgres://secret-user:secret-password@host:invalid/db?token=secret-query"},
			{"invalid escape", "postgresql://secret-user:secret-password@host/%zz"},
			{"missing host", "postgres:///secret-path?token=secret-query"},
			{"unsupported scheme", "https://secret-user:secret-password@host/db?token=secret-query"},
		} {
			t.Run(field+"/"+invalid.name, func(t *testing.T) {
				raw, err := json.Marshal(map[string]any{"PostgreSQL": []any{map[string]any{
					"name": "state", "credentials": map[string]string{field: invalid.value},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				_, err = database(string(raw), "state")
				if err == nil {
					t.Fatal("invalid PostgreSQL URL accepted")
				}
				message := err.Error()
				for _, expected := range []string{`binding "state"`, "credentials." + field, "postgres/postgresql URL with a host"} {
					if !strings.Contains(message, expected) {
						t.Errorf("missing requirement %q: %s", expected, message)
					}
				}
				if strings.Contains(message, "secret-") || strings.Contains(message, "<nil>") || strings.Contains(message, invalid.value) {
					t.Fatalf("unsafe or misleading PostgreSQL diagnostic: %s", message)
				}
			})
		}
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
