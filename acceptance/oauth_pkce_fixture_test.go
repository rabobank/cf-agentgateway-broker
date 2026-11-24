package acceptance_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Test subprocess standing in for xdg-open. Only follows the fixture's
// consent redirect to the actual plugin's loopback callback.
func init() {
	caFile := os.Getenv("AG_ACCEPTANCE_BROWSER_CA")
	if caFile == "" {
		return
	}
	ca, err := os.ReadFile(caFile)
	if err != nil || len(os.Args) != 2 {
		os.Exit(1)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		os.Exit(1)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if r.URL.Scheme != "http" || r.URL.Hostname() != "127.0.0.1" || r.URL.Path != "/callback" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := client.Get(os.Args[1])
	if err != nil || resp.StatusCode != 200 {
		os.Exit(1)
	}
	resp.Body.Close()
	os.Exit(0)
}

const oauthFixtureName = "oauth-pkce-mcp"
const fixtureOAuthAccessToken = "fixture-upstream-access-token"
const fixtureOAuthScope = "mcp:connect"
const fixtureClientSecret = "fixture-client-secret:+%"

func fixtureAccessToken(exchange int64) string {
	return fmt.Sprintf("%s-%d", fixtureOAuthAccessToken, exchange)
}

type oauthPKCEFixture struct {
	dynamic       bool
	registrations atomic.Int64
	server        *httptest.Server
	exchanges     atomic.Int64
	writes        atomic.Int64
}

func startOAuthPKCEFixture(work string, dynamic bool) *oauthPKCEFixture {
	GinkgoHelper()
	tlsKey, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	f := &oauthPKCEFixture{dynamic: dynamic}
	var mu sync.Mutex
	codes := map[string]url.Values{}
	tokens := map[string]int64{}
	sessions := map[string]int64{}
	clients := map[string]string{}
	var nextSession int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/resource-metadata", func(w http.ResponseWriter, r *http.Request) {
		stubJSON(w, 200, map[string]any{"resource": f.server.URL + "/mcp", "authorization_servers": []string{f.server.URL}, "scopes_supported": []string{fixtureOAuthScope}})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		metadata := map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true}
		if dynamic {
			metadata["registration_endpoint"] = f.server.URL + "/register"
			metadata["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic", "client_secret_post"}
		}
		stubJSON(w, 200, metadata)
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string   `json:"client_name"`
			Redirects []string `json:"redirect_uris"`
			Method    string   `json:"token_endpoint_auth_method"`
		}
		if !dynamic || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != oauthFixtureName || len(body.Redirects) != 1 || body.Method != "none" {
			w.WriteHeader(400)
			return
		}
		id := fmt.Sprintf("dynamic-client-%d", f.registrations.Add(1))
		mu.Lock()
		clients[id] = body.Redirects[0]
		mu.Unlock()
		stubJSON(w, 201, map[string]string{"client_id": id, "client_secret": fixtureClientSecret, "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		clientOK := !dynamic && q.Get("client_id") == oauthFixtureName || dynamic && clients[q.Get("client_id")] == q.Get("redirect_uri")
		mu.Unlock()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" || redirect.Path != "/callback" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" ||
			q.Get("response_type") != "code" || !clientOK || q.Get("scope") != fixtureOAuthScope || q.Get("resource") != f.server.URL+"/mcp" || q.Get("state") == "" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		code := base64.RawURLEncoding.EncodeToString([]byte(q.Get("state")))
		codes[code] = q
		mu.Unlock()
		redirect.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {f.server.URL}}.Encode()
		http.Redirect(w, r, redirect.String(), 302)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		code := r.PostForm.Get("code")
		q := codes[code]
		delete(codes, code)
		mu.Unlock()
		verifier := r.PostForm.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		clientOK := r.PostForm.Get("client_id") == q.Get("client_id") && r.Header.Get("Authorization") == ""
		if dynamic {
			id, secret, ok := r.BasicAuth()
			clientOK = ok && id == q.Get("client_id") && secret == url.QueryEscape(fixtureClientSecret) && !r.PostForm.Has("client_id") && !r.PostForm.Has("client_secret")
		}
		if q == nil || r.PostForm.Get("grant_type") != "authorization_code" || !clientOK || r.PostForm.Get("redirect_uri") != q.Get("redirect_uri") || r.PostForm.Get("resource") != q.Get("resource") || base64.RawURLEncoding.EncodeToString(hash[:]) != q.Get("code_challenge") {
			stubJSON(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		mu.Lock()
		exchange := f.exchanges.Add(1)
		token := fixtureAccessToken(exchange)
		tokens["Bearer "+token] = exchange
		mu.Unlock()
		stubJSON(w, 200, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
	})
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		exchange := tokens[r.Header.Get("Authorization")]
		mu.Unlock()
		if exchange == 0 {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.server.URL+`/oauth/resource-metadata", scope="`+fixtureOAuthScope+`"`)
			w.WriteHeader(401)
			return
		}
		var msg struct {
			ID     any
			Method string
			Params struct{ Name string }
		}
		if json.NewDecoder(r.Body).Decode(&msg) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		if msg.Method == "initialize" {
			nextSession++
			id := fmt.Sprintf("fixture-session-%d", nextSession)
			sessions[id] = exchange
			w.Header().Set("Mcp-Session-Id", id)
		} else if sessions[r.Header.Get("Mcp-Session-Id")] != exchange {
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Unlock()
		if msg.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": oauthFixtureName, "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{fixtureTool("read_design"), fixtureTool("write_design")}}
		case "tools/call":
			if msg.Params.Name == "write_design" {
				f.writes.Add(1)
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": fmt.Sprintf("upstream-ok: authorization %d", exchange)}}}
		default:
			w.WriteHeader(400)
			return
		}
		stubJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	})
	f.server = httptest.NewUnstartedServer(mux)
	startFixtureTLS(f.server, work, tlsKey)
	return f
}

func fixtureTool(name string) any {
	return map[string]any{"name": name, "inputSchema": map[string]string{"type": "object"}}
}

func (f *oauthPKCEFixture) gatewayConfig() map[string]any {
	return map[string]any{"session_key": strings.Repeat("34", 32), "oauth_updates": map[string]string{"encryption_key": strings.Repeat("56", 32)}, "backends": []any{map[string]any{"name": oauthFixtureName, "revision": "v1", "team_oauth": map[string]any{"allowed_tools": []string{"read_design"}}, "mcp": map[string]any{"targets": []any{map[string]any{"name": "tools", "mcp": map[string]string{"host": f.server.URL + "/mcp"}}}}}}}
}

func (f *oauthPKCEFixture) offering(port int) map[string]any {
	profile := map[string]any{"version": 1, "resource": f.server.URL + "/mcp", "issuer": f.server.URL, "scopes": []string{fixtureOAuthScope}}
	if f.dynamic {
		profile["registration"] = map[string]string{"client_name": oauthFixtureName}
	} else {
		profile["client_id"] = oauthFixtureName
	}
	return map[string]any{"id": "team-mcp", "name": "team-mcp", "description": "Team MCP", "protocol": "mcp", "connection": map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d/mcp", port)}, "plans": []any{map[string]any{"id": oauthFixtureName, "name": oauthFixtureName, "description": "OAuth with PKCE", "access": map[string]any{"backends": []string{oauthFixtureName}}, "oauth_profile": profile}}}
}
