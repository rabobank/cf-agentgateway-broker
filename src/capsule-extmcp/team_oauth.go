package main

import (
	"bytes"
	"cf-agentgateway-capsule/plugin"
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const oauthKind = "mcp.oauthConnection"
const bindingHeader = "X-Agw-Mcp-Binding"

type oauthBackend struct {
	revision string
	endpoint string
	tools    map[string]bool
}

type teamDashboard struct {
	logID    string
	aead     cipher.AEAD
	client   *http.Client
	admin    string
	backends map[string]oauthBackend
}

type oauthResource struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}

type oauthSnapshot struct {
	Resources  []oauthResource `json:"resources"`
	Generation *int64          `json:"generation"`
}

type oauthConnection struct {
	Name       string `json:"name"`
	Generation string `json:"instanceGeneration"`
	Backend    string `json:"backend"`
	Revision   string `json:"backendRevision"`
	Sealed     string `json:"sealed,omitempty"`
	Operation  string `json:"operation,omitempty"`
	Previous   string `json:"previous,omitempty"`
	Sequence   uint64 `json:"sequence,omitempty"`
}

type upstreamSecret struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
	Epoch   string    `json:"epoch"`
}

func newTeamDashboard(cfg mcpconfig.GatewayOperator) (*teamDashboard, error) {
	if cfg.MCP.OAuthUpdates == nil {
		return nil, nil
	}
	if err := cfg.MCP.OAuthUpdates.Validate(); err != nil {
		return nil, err
	}
	key, _ := hex.DecodeString(cfg.MCP.OAuthUpdates.StorageKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	d := &teamDashboard{logID: cfg.Capsules.LogID, aead: aead, admin: "http://127.0.0.1:15000",
		client:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		backends: map[string]oauthBackend{}}
	if !mcpconfig.ValidID(d.logID) {
		return nil, errors.New("team OAuth requires a valid installation log ID")
	}
	for _, backend := range cfg.MCP.Backends {
		if backend.TeamOAuth == nil {
			continue
		}
		if err := backend.TeamOAuth.Validate(); err != nil {
			return nil, err
		}
		if !mcpconfig.ValidID(backend.Name) || !mcpconfig.ValidID(backend.Revision) || len(backend.MCP.Targets) != 1 || d.backends[backend.Name].endpoint != "" {
			return nil, errors.New("invalid team OAuth backend")
		}
		mcp, _ := backend.MCP.Targets[0]["mcp"].(map[string]any)
		endpoint, _ := mcp["host"].(string)
		if !mcpconfig.OAuthURL(endpoint) {
			return nil, errors.New("team OAuth requires an HTTPS upstream")
		}
		tools := map[string]bool{}
		for _, tool := range backend.TeamOAuth.AllowedTools {
			tools[tool] = true
		}
		d.backends[backend.Name] = oauthBackend{backend.Revision, endpoint, tools}
	}
	return d, nil
}

func runTeamOAuth(ctx context.Context) error {
	var cfg mcpconfig.GatewayOperator
	if raw := os.Getenv("AGENTGATEWAY_CONFIG"); raw != "" {
		if err := mcpconfig.DecodeOperator([]byte(raw), &cfg); err != nil {
			return err
		}
	}
	d, err := newTeamDashboard(cfg)
	if err != nil {
		return err
	}
	if d == nil {
		<-ctx.Done()
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", d.authorize)
	mux.HandleFunc("/upstream/{backend}", d.upstream)
	mux.HandleFunc("GET /internal/mcp-oauth/{instance}", d.connectionStatus)
	mux.HandleFunc("PATCH /internal/mcp-oauth/{instance}", d.updateConnection)
	listener, err := net.Listen("tcp", "127.0.0.1:50053")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	defer srv.Close()
	result := make(chan error, 1)
	go func() { result <- srv.Serve(listener) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-result:
		return err
	}
}

func readJSON(r io.Reader, limit int64, out any) error {
	decoder := json.NewDecoder(io.LimitReader(r, limit))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func (d *teamDashboard) snapshot(ctx context.Context, kind, id string) (oauthSnapshot, error) {
	var out oauthSnapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.admin+"/api/config/resources/"+kind+"/"+url.PathEscape(id), nil)
	if err != nil {
		return out, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, errors.New("management read failed")
	}
	return out, readJSON(resp.Body, 16<<20, &out)
}

func (s oauthSnapshot) find(id string, out any) bool {
	for _, r := range s.Resources {
		if r.ID == id {
			return json.Unmarshal(r.Value, out) == nil
		}
	}
	return false
}

func (d *teamDashboard) writeConnection(ctx context.Context, snapshot oauthSnapshot, c oauthConnection) bool {
	if snapshot.Generation == nil {
		return false
	}
	body, _ := json.Marshal(map[string]any{"resources": []any{map[string]any{"value": c}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.admin+"/api/config/resources/"+oauthKind, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(*snapshot.Generation, 10)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func (d *teamDashboard) seal(purpose string, value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, d.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(d.aead.Seal(nonce, nonce, body, []byte(purpose))), nil
}

func (d *teamDashboard) open(purpose, sealed string, out any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	n := d.aead.NonceSize()
	if err != nil || len(raw) < n || len(raw) > 24<<10 {
		return false
	}
	plain, err := d.aead.Open(nil, raw[:n], raw[n:], []byte(purpose))
	return err == nil && json.Unmarshal(plain, out) == nil
}

func nonce() (string, error) {
	var b [32]byte
	_, err := rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:]), err
}

func (d *teamDashboard) instance(ctx context.Context, id string) (mcpconfig.Instance, bool) {
	i, _, ok := d.instanceAt(ctx, id)
	return i, ok
}

func (d *teamDashboard) instanceAt(ctx context.Context, id string) (mcpconfig.Instance, *int64, bool) {
	var value struct{ Metadata mcpconfig.Instance }
	s, err := d.snapshot(ctx, "mcp.instance", id)
	i := &value.Metadata
	if err != nil || !s.find(id, &value) || i.Installation != d.logID || i.Revoked || i.Generation == "" ||
		i.Backend.Auth.Method != mcpconfig.TeamOAuthMethod || len(i.Selections) != 1 || id != mcpconfig.ManagedID(d.logID, i.ID, "") ||
		i.Selections[0].Revision != d.backends[i.Selections[0].Name].revision || d.backends[i.Selections[0].Name].endpoint == "" {
		return mcpconfig.Instance{}, nil, false
	}
	return *i, s.Generation, true
}

func connectionPurpose(c oauthConnection) string {
	return "upstream-token/v1/" + c.Name + "/" + c.Generation + "/" + c.Backend + "/" + c.Revision
}

func (d *teamDashboard) usable(s oauthSnapshot, id string, i mcpconfig.Instance) (upstreamSecret, bool) {
	var c oauthConnection
	var token upstreamSecret
	if !s.find(id, &c) || c.Name != id || c.Generation != i.Generation || c.Backend != i.Selections[0].Name || c.Revision != i.Selections[0].Revision ||
		!d.open(connectionPurpose(c), c.Sealed, &token) || token.Epoch == "" || !token.Expires.After(time.Now()) || !validToken(token.Token) {
		return upstreamSecret{}, false
	}
	return token, true
}

func (d *teamDashboard) grant(ctx context.Context, instance, binding, target, header string) (mcpconfig.Binding, upstreamSecret, bool) {
	denied := mcpconfig.Binding{}
	if !mcpconfig.ValidID(instance) || !mcpconfig.ValidID(binding) || !strings.HasPrefix(header, "Bearer ") || len(header) != 71 {
		return denied, upstreamSecret{}, false
	}
	parent := mcpconfig.ManagedID(d.logID, instance, "")
	s, err := d.snapshot(ctx, "mcp.grant", mcpconfig.ManagedID(d.logID, instance, binding))
	var g struct {
		Instance string
		Metadata mcpconfig.Binding
	}
	if err != nil || !s.find(mcpconfig.ManagedID(d.logID, instance, binding), &g) || g.Instance != parent {
		return denied, upstreamSecret{}, false
	}
	b := g.Metadata
	hash := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	if b.Revoked || b.TargetName() != target || b.ID != binding || b.Instance.ID != instance || b.Instance.Installation != d.logID || b.App == "" ||
		subtle.ConstantTimeCompare([]byte(b.BearerHash), []byte(hex.EncodeToString(hash[:]))) != 1 {
		return denied, upstreamSecret{}, false
	}
	i, gen, ok := d.instanceAt(ctx, parent)
	if !ok || i.Generation != b.Instance.Generation {
		return denied, upstreamSecret{}, false
	}
	connections, err := d.snapshot(ctx, oauthKind, parent)
	if err != nil || gen == nil || s.Generation == nil || connections.Generation == nil || *gen != *s.Generation || *gen != *connections.Generation {
		return denied, upstreamSecret{}, false
	}
	secret, ok := d.usable(connections, parent, i)
	return b, secret, ok
}

type bindingContext struct {
	Binding  string `json:"binding_id"`
	Instance string `json:"instance_id"`
	Target   string `json:"target"`
}

func singleHeader(h http.Header, name string) string {
	if values := h.Values(name); len(values) == 1 {
		return values[0]
	}
	return ""
}

func (d *teamDashboard) authorize(w http.ResponseWriter, r *http.Request) {
	var in bindingContext
	if readJSON(http.MaxBytesReader(w, r.Body, 8192), 8192, &in) != nil {
		http.Error(w, "binding denied", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1800*time.Millisecond)
	defer cancel()
	b, _, ok := d.grant(ctx, in.Instance, in.Binding, in.Target, singleHeader(r.Header, "Authorization"))
	identities, err := b.TargetIdentities()
	if !ok || err != nil {
		http.Error(w, "binding denied", 403)
		return
	}
	context, _ := json.Marshal(in)
	w.Header().Set(bindingHeader, base64.RawURLEncoding.EncodeToString(context))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(plugin.Identity{Installation: d.logID, Instance: b.Instance.ID, Binding: b.ID, Target: b.TargetName(), Org: b.Instance.Org, Space: b.Instance.Space, SelectedIdentity: b.Selected(), TargetIdentities: identities})
}

func (d *teamDashboard) upstream(w http.ResponseWriter, r *http.Request) {
	raw, err := base64.RawURLEncoding.DecodeString(singleHeader(r.Header, bindingHeader))
	var binding bindingContext
	if err != nil || json.Unmarshal(raw, &binding) != nil {
		http.Error(w, "binding denied", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	b, secret, ok := d.grant(ctx, binding.Instance, binding.Binding, binding.Target, singleHeader(r.Header, "Authorization"))
	cancel()
	backend := r.PathValue("backend")
	if !ok || len(b.Instance.Selections) != 1 || b.Instance.Selections[0].Name != backend || b.Instance.Selections[0].Revision != d.backends[backend].revision {
		http.Error(w, "binding denied", 403)
		return
	}
	up := d.backends[backend]
	var body []byte
	switch r.Method {
	case http.MethodPost:
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		var canonical bool
		body, canonical = canonicalMCP(body)
		if err != nil || !canonical || !allowedMCP(body, up.tools) {
			http.Error(w, "MCP action denied", 403)
			return
		}
	case http.MethodDelete:
	case http.MethodGet:
		// Dial on demand with POST. Don't retain a separate upstream SSE listener.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, cancel = context.WithDeadline(r.Context(), minTime(time.Now().Add(2*time.Minute), secret.Expires))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, up.endpoint, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "upstream unavailable", 502)
		return
	}
	for _, name := range []string{"Accept", "Content-Type", "MCP-Protocol-Version", "Last-Event-ID"} {
		if value := singleHeader(r.Header, name); value != "" {
			req.Header.Set(name, value)
		}
	}
	const sessionHeader = "Mcp-Session-Id"
	purpose := "upstream-session/v1/" + b.TargetName() + "/" + secret.Epoch
	if len(r.Header.Values(sessionHeader)) > 1 {
		http.Error(w, "ambiguous MCP session", 400)
		return
	}
	if raw := singleHeader(r.Header, sessionHeader); raw != "" {
		var session string
		if !d.open(purpose, raw, &session) || !validToken(session) {
			http.Error(w, "MCP session expired", 404)
			return
		}
		req.Header.Set(sessionHeader, session)
	}
	req.Header.Set("Authorization", "Bearer "+secret.Token)
	resp, err := d.client.Do(req)
	if err != nil {
		http.Error(w, "upstream unavailable", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		http.Error(w, "upstream redirects are unsupported", 502)
		return
	}
	if resp.StatusCode >= 400 {
		http.Error(w, "upstream MCP request rejected", resp.StatusCode)
		return
	}
	if session := singleHeader(resp.Header, sessionHeader); session != "" {
		sealed, err := d.seal(purpose, session)
		if err != nil {
			http.Error(w, "upstream session unavailable", 502)
			return
		}
		w.Header().Set(sessionHeader, sealed)
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_, _ = io.Copy(flushResponse{w}, resp.Body)
}

type flushResponse struct{ http.ResponseWriter }

func (w flushResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func allowedMCP(body []byte, tools map[string]bool) bool {
	var request struct {
		JSONRPC string                `json:"jsonrpc"`
		Method  string                `json:"method"`
		Params  struct{ Name string } `json:"params"`
	}
	if json.Unmarshal(body, &request) != nil || request.JSONRPC != "2.0" {
		return false
	}
	switch request.Method {
	case "initialize", "notifications/initialized", "notifications/cancelled", "ping", "tools/list":
		return true
	case "tools/call":
		return tools[request.Params.Name]
	default:
		return false
	}
}

func canonicalMCP(body []byte) ([]byte, bool) {
	// Re-encode before forwarding so the allowlist and upstream cannot disagree
	// on duplicate keys, non-canonical field names, or a batch envelope.
	var message map[string]json.RawMessage
	if json.Unmarshal(body, &message) != nil || message == nil {
		return nil, false
	}
	for key := range message {
		switch key {
		case "jsonrpc", "method", "id", "params":
		default:
			return nil, false
		}
	}
	if raw, ok := message["params"]; ok {
		var params map[string]json.RawMessage
		if json.Unmarshal(raw, &params) != nil || params == nil {
			return nil, false
		}
		for key := range params {
			if key != "name" && strings.EqualFold(key, "name") {
				return nil, false
			}
		}
		message["params"], _ = json.Marshal(params)
	}
	out, err := json.Marshal(message)
	return out, err == nil
}

func validToken(s string) bool {
	return s != "" && len(s) <= 8192 && strings.IndexFunc(s, func(r rune) bool { return r < 33 || r > 126 }) == -1
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
