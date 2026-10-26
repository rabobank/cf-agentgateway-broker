package main

import "testing"

func TestDatabaseBinding(t *testing.T) {
	raw := `{"PostgreSQL":[{"name":"agentgateway-state","credentials":{"uri":"postgres://user:password@database.apps.internal:5432/postgres?sslmode=disable"}}]}`
	uri, err := database(raw)
	if err != nil || uri == "" {
		t.Fatal("binding was not resolved")
	}
	for _, bad := range []string{`{}`, `{"PostgreSQL":[{"name":"other","credentials":{"uri":"postgres://db/x"}}]}`, `{"PostgreSQL":[{"name":"agentgateway-state","credentials":{"uri":"https://db/x"}}]}`} {
		if _, err = database(bad); err == nil {
			t.Fatal("invalid binding accepted")
		}
	}
}
