package main

import (
	mcpconfig "cf-mcp-config"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtectedResourceDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, advertised, served string
		wrongIssuer              bool
	}{
		{"advertised", "/custom", "/custom", false},
		{"path fallback", "/unavailable", "/.well-known/oauth-protected-resource/mcp", false},
		{"root fallback", "http://invalid.example", "/.well-known/oauth-protected-resource", false},
		{"issuer mismatch", "/custom", "/custom", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				origin := "https://" + r.Host
				switch r.URL.Path {
				case "/mcp":
					if r.Method != "POST" || r.Header.Get("Authorization") != "" {
						t.Error("invalid MCP discovery probe")
					}
					advertised := tc.advertised
					if advertised[0] == '/' {
						advertised = origin + advertised
					}
					w.Header().Set("WWW-Authenticate", `Basic realm="other, realm", Bearer scope="test", resource_metadata="`+advertised+`"`)
					w.WriteHeader(401)
				case tc.served:
					issuer := origin
					if tc.wrongIssuer {
						issuer = "https://unexpected.example"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"resource": origin + "/mcp", "authorization_servers": []string{issuer}})
				default:
					w.WriteHeader(404)
				}
			}))
			server.StartTLS()
			defer server.Close()
			original := oauthHTTP
			oauthHTTP = server.Client()
			defer func() { oauthHTTP = original }()
			err := discoverProtectedResource(t.Context(), mcpconfig.OAuthProfile{Resource: server.URL + "/mcp", Issuer: server.URL})
			if (err != nil) != tc.wrongIssuer {
				t.Fatalf("unexpected discovery result: %v", err)
			}
		})
	}
}
