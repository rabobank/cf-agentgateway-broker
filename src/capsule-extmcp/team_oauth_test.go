package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTeamOAuthBindingAndUpstreamIsolation(t *testing.T) {
	const consumer = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const access = "upstream-only-token"
	var upstreamCalls atomic.Int32
	var session atomic.Value
	session.Store("")
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+access || r.Header.Get(bindingHeader) != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Mcp-Session-Id") != session.Load().(string) {
			t.Error("wrong or leaked upstream credentials or session")
		}
		w.Header().Set("Mcp-Session-Id", "provider-session")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer provider.Close()
	var cfg mcpconfig.GatewayOperator
	cfg.Capsules.LogID = "lab"
	cfg.MCP.OAuthUpdates = &mcpconfig.OAuthUpdates{StorageKey: strings.Repeat("12", 32)}
	backend := mcpconfig.SharedBackend{Name: "test", Revision: "v1", TeamOAuth: &mcpconfig.TeamOAuth{AllowedTools: []string{"read_design"}}}
	backend.MCP.Targets = []map[string]any{{"name": "design", "mcp": map[string]any{"host": provider.URL}}}
	cfg.MCP.Backends = []mcpconfig.SharedBackend{backend}
	i := mcpconfig.Instance{ID: "i", Installation: "lab", Generation: "gen", Backend: mcpconfig.Backend{Auth: mcpconfig.Auth{Method: mcpconfig.TeamOAuthMethod}}, Selections: []mcpconfig.BackendSelection{{Name: "test", Revision: "v1"}}}
	parent := mcpconfig.ManagedID("lab", "i", "")
	hash := sha256.Sum256([]byte(consumer))
	binding := mcpconfig.Binding{ID: "b", Instance: i, App: "app", Generation: "bindGen", BearerHash: hex.EncodeToString(hash[:])}
	grant := mcpconfig.ManagedID("lab", "i", "b")

	d, err := newTeamDashboard(cfg)
	if err != nil {
		t.Fatal(err)
	}
	connection := func(d *teamDashboard, epoch string) oauthConnection {
		t.Helper()
		c := oauthConnection{Name: parent, Backend: "test", Revision: "v1", Generation: "gen"}
		c.Sealed, err = d.seal(connectionPurpose(c), upstreamSecret{Token: access, Expires: time.Now().Add(time.Hour), Epoch: epoch})
		if err != nil || strings.Contains(c.Sealed, access) {
			t.Fatal("connection encryption failed")
		}
		return c
	}
	var saved atomic.Value
	saved.Store(connection(d, "first"))
	var revoked atomic.Bool
	var changedGeneration atomic.Bool
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var resources []any
		generation := 1
		appendResource := func(id string, value any) { resources = append(resources, map[string]any{"id": id, "value": value}) }
		switch strings.TrimPrefix(r.URL.Path, "/api/config/resources/") {
		case "mcp.instance/" + parent:
			appendResource(parent, map[string]any{"metadata": i})
		case "mcp.grant/" + grant:
			if !revoked.Load() {
				appendResource(grant, map[string]any{"instance": parent, "metadata": binding})
			}
		case oauthKind + "/" + parent:
			appendResource(parent, saved.Load())
			if changedGeneration.Load() {
				generation++
			}
		default:
			t.Errorf("unexpected management read: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": resources, "generation": generation})
	}))
	defer management.Close()
	d.admin, d.client.Transport = management.URL, provider.Client().Transport
	// A fresh replica can authorize without any in-memory grant or credential copy.
	replica, _ := newTeamDashboard(cfg)
	replica.admin, replica.client.Transport = management.URL, provider.Client().Transport
	ctx := fmt.Sprintf(`{"binding_id":"b","instance_id":"i","target":%q}`, binding.TargetName())
	auth := func(bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/authorize", strings.NewReader(ctx))
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		replica.authorize(w, r)
		return w
	}
	if w := auth(strings.Repeat("f", 64)); w.Code != 403 {
		t.Fatal("wrong agent credential accepted")
	}
	changedGeneration.Store(true)
	if w := auth(consumer); w.Code != 403 {
		t.Fatal("mixed management generations accepted")
	}
	changedGeneration.Store(false)
	w := auth(consumer)
	if w.Code != 200 || strings.Contains(w.Body.String(), access) || strings.Contains(w.Body.String(), consumer) {
		t.Fatal("live binding authorization or secret isolation failed")
	}
	marker := w.Header().Get(bindingHeader)
	proxy := func(body, sid string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/upstream/test", strings.NewReader(body))
		r.SetPathValue("backend", "test")
		r.Header.Set("Authorization", "Bearer "+consumer)
		r.Header.Set("Cookie", "browser=secret")
		r.Header.Set(bindingHeader, marker)
		if sid != "" {
			r.Header.Set("Mcp-Session-Id", sid)
		}
		w := httptest.NewRecorder()
		replica.upstream(w, r)
		return w
	}
	const read = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_design"}}`
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_design"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"read_design"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_design","Name":"write_design"}}`,
		`[` + read + `]`,
	} {
		if w := proxy(body, ""); w.Code != 403 || upstreamCalls.Load() != 0 {
			t.Fatal("denied operation reached upstream")
		}
	}
	w = proxy(read, "")
	sid := w.Header().Get("Mcp-Session-Id")
	if w.Code != 200 || sid == "" || sid == "provider-session" {
		t.Fatalf("expected isolated upstream session, HTTP %d", w.Code)
	}
	session.Store("provider-session")
	if w := proxy(read, sid); w.Code != 200 {
		t.Fatal("valid session could not be resumed")
	}
	saved.Store(connection(d, "replacement"))
	if w := proxy(read, sid); w.Code != 404 {
		t.Fatal("previous credential epoch resumed")
	}
	saved.Store(oauthConnection{Name: parent, Backend: "test", Revision: "v1", Generation: "gen"})
	if w := proxy(read, sid); w.Code != 403 {
		t.Fatal("disconnect did not revoke upstream access")
	}
	saved.Store(connection(d, "reconnected"))
	revoked.Store(true)
	if w := proxy(read, ""); w.Code != 403 || upstreamCalls.Load() != 2 {
		t.Fatal("unbind did not revoke upstream access")
	}
}

func TestTeamOAuthNativeGateway(t *testing.T) {
	binary := os.Getenv("TEAM_OAUTH_GATEWAY_BINARY")
	if binary == "" {
		t.Skip("set TEAM_OAUTH_GATEWAY_BINARY to the rebuilt gateway")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	openPort := func() int {
		t.Helper()
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const marker = "trusted-binding-context"
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(403)
			return
		}
		w.Header().Set(bindingHeader, marker)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if r.Header.Get(bindingHeader) != marker || r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("gateway did not deliver the authorized request context to its upstream connector")
			w.WriteHeader(403)
			return
		}
		var msg struct {
			ID     any
			Method string
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "mock", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read_design", "inputSchema": map[string]string{"type": "object"}}, map[string]any{"name": "write_design", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			calls.Add(1)
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "read-ok"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	port, admin := openPort(), openPort()
	work := t.TempDir()
	binding := mcpconfig.Binding{ID: "binding", Instance: mcpconfig.Instance{ID: "instance", Backend: mcpconfig.Backend{Name: "design", URL: server.URL + "/mcp", Auth: mcpconfig.Auth{Method: mcpconfig.TeamOAuthMethod}}}}
	route := binding.Route()
	pol := route["policies"].(map[string]any)
	delete(pol, "mcpGuardrails")
	pol["extAuthz"].(map[string]any)["host"] = strings.TrimPrefix(server.URL, "http://")
	targets := route["backends"].([]any)[0].(map[string]any)["mcp"].(map[string]any)["targets"]
	route["backends"] = []any{}
	config := map[string]any{"config": map[string]any{"adminAddr": fmt.Sprintf("127.0.0.1:%d", admin), "statsAddr": fmt.Sprintf("127.0.0.1:%d", openPort()), "readinessAddr": fmt.Sprintf("127.0.0.1:%d", openPort()), "storage": map[string]string{"mode": "hybrid"}, "database": map[string]string{"url": "sqlite://" + filepath.Join(work, "state.db") + "?mode=rwc"}, "session": map[string]string{"key": strings.Repeat("12", 32)}}, "gateways": map[string]any{"inference": map[string]any{"port": port}}, "mcpBackends": []any{map[string]any{"name": "design", "revision": "v1", "targets": targets, "teamOAuth": true, "allowedTools": []string{"read_design"}}}}
	data, _ := json.Marshal(config)
	path := filepath.Join(work, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "-f", path)
	cmd.Env = append(os.Environ(), "RUST_MIN_STACK=16777216")
	log, err := os.Create(filepath.Join(work, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	})
	client := &http.Client{Timeout: 4 * time.Second}
	management := fmt.Sprintf("http://127.0.0.1:%d/api/config/resources/%s/opaque", admin, oauthKind)
	var snap oauthSnapshot
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		resp, e := client.Get(management)
		if e == nil {
			e = readJSON(resp.Body, 8192, &snap)
			resp.Body.Close()
			if resp.StatusCode == 200 && e == nil && snap.Generation != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway management API never became ready")
		}
	}
	d := &teamDashboard{client: client, admin: fmt.Sprintf("http://127.0.0.1:%d", admin)}
	resource := oauthConnection{Name: "opaque", Sealed: "opaque-ciphertext"}
	if !d.writeConnection(ctx, snap, resource) || d.writeConnection(ctx, snap, resource) {
		t.Fatal("native conditional persistence accepted stale generation or rejected a current generation")
	}
	if current, err := d.snapshot(ctx, oauthKind, "opaque"); err != nil || !current.find("opaque", &resource) {
		t.Fatal("native storage did not persist OAuth connection")
	}
	manage := func(kind string, value any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"resources": []any{map[string]any{"value": value}}})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, d.admin+"/api/config/resources/"+kind, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("manage %s: %d %s", kind, resp.StatusCode, raw)
		}
	}
	manage("mcp.instance", map[string]any{"name": "instance", "metadata": binding.Instance})
	manage("mcp.grant", map[string]any{"name": "binding", "instance": "instance", "metadata": binding, "route": route,
		"backendSelections": []mcpconfig.BackendSelection{{Name: "design", Revision: "v1", TargetPrefix: "test", Parameters: map[string]string{}}}})
	var session string
	call := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d%s", port, binding.Path()), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set(bindingHeader, "spoofed")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Mcp-Session-Id"); got != "" {
			session = got
		}
		raw, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		return resp.StatusCode, string(raw)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		code, body := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`)
		if code == 200 && strings.Contains(body, "serverInfo") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initialize: %d %s", code, body)
		}
	}
	call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code, body := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); code != 200 || !strings.Contains(body, "read_design") || strings.Contains(body, "write_design") {
		t.Fatalf("tools/list: %d %s", code, body)
	}
	call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_design","arguments":{}}}`)
	if calls.Load() != 0 {
		t.Fatal("native gateway forwarded a denied tool call")
	}
	if code, body := call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_design","arguments":{}}}`); code != 200 || !strings.Contains(body, "read-ok") || calls.Load() != 1 {
		t.Fatalf("allowed call did not reach upstream: %d %s", code, body)
	}
}
