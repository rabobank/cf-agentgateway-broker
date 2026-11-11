package main

import (
	"bytes"
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const labelPrefix = "cf.agentgateway-broker/"

type Config struct {
	Listen          string              `json:"listen"`
	Username        string              `json:"username"`
	Password        string              `json:"password"`
	Installation    string              `json:"installation"`
	GatewayURL      string              `json:"gateway_url"`
	MCPURL          string              `json:"mcp_url"`
	ManagementURL   string              `json:"management_url"`
	ManagementToken string              `json:"management_token"`
	CAFile          string              `json:"ca_file"`
	TrustedCAFile   string              `json:"trusted_ca_file"`
	Certificate     string              `json:"certificate"`
	PrivateKey      string              `json:"private_key"`
	ServiceID       string              `json:"service_id"`
	PlanID          string              `json:"plan_id"`
	Models          []string            `json:"models"`
	Budget          json.Number         `json:"budget"`
	Window          string              `json:"window"`
	MCPServers      []mcpconfig.Backend `json:"mcp_servers,omitempty"`
}

type Resource struct {
	ID    string `json:"id"`
	Value Key    `json:"value"`
}
type Key struct {
	Key           string         `json:"key"`
	Metadata      map[string]any `json:"metadata"`
	AllowedModels []string       `json:"allowedModels"`
	Budgets       []Budget       `json:"budgets"`
}
type Budget struct {
	Name   string            `json:"name"`
	Limit  Limit             `json:"limit"`
	Window map[string]string `json:"window"`
	Action string            `json:"onBudgetExceeded"`
}
type Limit struct {
	Unit   string      `json:"unit"`
	Amount json.Number `json:"amount"`
}
type Request struct {
	ServiceID        string         `json:"service_id"`
	PlanID           string         `json:"plan_id"`
	OrganizationGUID string         `json:"organization_guid"`
	SpaceGUID        string         `json:"space_guid"`
	Parameters       map[string]any `json:"parameters"`
	Context          map[string]any `json:"context"`
	BindResource     map[string]any `json:"bind_resource"`
}
type Broker struct {
	cfg       Config
	client    *http.Client
	trustedCA string
}
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

var resourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func (c Config) validate() error {
	if err := mcpconfig.Validate(c.MCPServers, c.PlanID); err != nil {
		return err
	}
	if len(c.MCPServers) > 0 && c.MCPURL == "" {
		return errors.New("MCP URL required with MCP servers")
	}
	if c.MCPURL != "" {
		u, err := url.Parse(c.MCPURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return errors.New("invalid MCP URL")
		}
	}
	for name, value := range map[string]string{"listen": c.Listen, "username": c.Username, "password": c.Password, "installation": c.Installation, "service_id": c.ServiceID, "plan_id": c.PlanID, "window": c.Window} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	for _, value := range []string{c.GatewayURL, c.ManagementURL} {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid gateway URL")
		}
	}
	amount, err := c.Budget.Float64()
	if err != nil || amount <= 0 {
		return errors.New("positive USD budget is required")
	}
	if len(c.Models) == 0 {
		return errors.New("model allowlist is required")
	}
	for _, model := range c.Models {
		if model == "" || strings.Contains(model, "*") {
			return errors.New("model aliases must be explicit")
		}
	}
	if (c.Certificate == "") != (c.PrivateKey == "") {
		return errors.New("certificate and private key must be configured together")
	}
	return nil
}
func newBroker(c Config) (*Broker, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid management CA")
		}
		tlsConfig.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	var trustedCA string
	if c.TrustedCAFile != "" {
		pem, err := os.ReadFile(c.TrustedCAFile)
		if err != nil {
			return nil, errors.New("cannot read inference trust CA")
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid inference trust CA")
		}
		trustedCA = string(pem)
	}
	return &Broker{cfg: c, client: &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, trustedCA: trustedCA}, nil
}
func (b *Broker) gateway(ctx context.Context, method, path string, body any, out any) error {
	return b.management(ctx, method, "llm.apiKey", path, body, out)
}
func (b *Broker) management(ctx context.Context, method, kind, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.cfg.ManagementURL, "/")+"/api/config/resources/"+kind+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.cfg.ManagementToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.ManagementToken)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New("gateway management request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{resp.StatusCode, "gateway management request rejected"}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<20))
	return err
}
func (b *Broker) find(ctx context.Context, id string) (*Resource, error) {
	var list struct {
		Resources []Resource `json:"resources"`
	}
	if err := b.gateway(ctx, http.MethodGet, "", nil, &list); err != nil {
		return nil, err
	}
	var found *Resource
	for i := range list.Resources {
		r := &list.Resources[i]
		if r.Value.Metadata[labelPrefix+"instance"] == id && r.Value.Metadata[labelPrefix+"installation"] == b.cfg.Installation {
			if found != nil {
				return nil, errors.New("duplicate service-instance resources")
			}
			found = r
		}
	}
	return found, nil
}
func fingerprint(req Request) string {
	// CAPI context may evolve independently; the provision contract is IDs, org/space and parameters.
	if len(req.Parameters) == 0 {
		req.Parameters = nil
	}
	data, _ := json.Marshal(struct {
		Service, Plan, Org, Space string
		Parameters                map[string]any
	}{req.ServiceID, req.PlanID, req.OrganizationGUID, req.SpaceGUID, req.Parameters})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	write(w, status, map[string]string{"description": message})
}
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		write(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/ready" && r.Method == http.MethodGet {
		if _, err := b.find(r.Context(), "readiness"); err != nil {
			fail(w, 503, "gateway management unavailable")
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
		return
	}
	user, password, ok := r.BasicAuth()
	userHash, expectedUser := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(b.cfg.Username))
	passHash, expectedPass := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(b.cfg.Password))
	if !ok || subtle.ConstantTimeCompare(userHash[:], expectedUser[:]) != 1 || subtle.ConstantTimeCompare(passHash[:], expectedPass[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="broker"`)
		fail(w, 401, "authentication required")
		return
	}
	version := strings.Split(r.Header.Get("X-Broker-API-Version"), ".")
	minor := 0
	if len(version) == 2 {
		minor, _ = strconv.Atoi(version[1])
	}
	if len(version) != 2 || version[0] != "2" || minor < 15 {
		write(w, 412, map[string]string{"error": "PreconditionFailed", "description": "OSB API version 2.15 or newer is required"})
		return
	}
	if r.URL.Path == "/v2/catalog" && r.Method == http.MethodGet {
		b.catalog(w)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v2" || parts[1] != "service_instances" || !resourceID.MatchString(parts[2]) {
		fail(w, 404, "not found")
		return
	}
	if b.serveMCP(w, r, parts) {
		return
	}
	if len(b.cfg.MCPServers) > 0 && (r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch) {
		instance, err := b.instance(r.Context(), parts[2])
		if err != nil {
			fail(w, 503, "cannot resolve instance ownership")
			return
		}
		if instance != nil {
			fail(w, 409, "instance belongs to another service plan")
			return
		}
	}
	id := parts[2]
	var req Request
	if r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&req); err != nil {
			fail(w, 400, "invalid request JSON")
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			fail(w, 400, "invalid request JSON")
			return
		}
	} else if r.Method == http.MethodDelete {
		req.ServiceID = r.URL.Query().Get("service_id")
		req.PlanID = r.URL.Query().Get("plan_id")
	}
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		if req.ServiceID != b.cfg.ServiceID || req.PlanID != b.cfg.PlanID {
			fail(w, 400, "unknown service or plan")
			return
		}
		if len(req.Parameters) > 0 {
			fail(w, 400, "custom parameters are not supported")
			return
		}
	}
	if len(parts) == 3 && r.Method == http.MethodPatch {
		fail(w, 422, "plan updates are not supported")
		return
	}
	if len(parts) != 3 && !(len(parts) == 5 && parts[3] == "service_bindings" && resourceID.MatchString(parts[4])) {
		fail(w, 404, "not found")
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		fail(w, 405, "method not supported")
		return
	}
	existing, err := b.find(r.Context(), id)
	if err != nil {
		fail(w, 503, "cannot resolve service instance in gateway")
		return
	}
	if len(parts) == 5 {
		if r.Method == http.MethodDelete {
			write(w, 200, map[string]any{})
			return
		}
		if existing == nil {
			fail(w, 404, "service instance not found")
			return
		}
		if existing.Value.Key == "" {
			fail(w, 503, "instance credential is not recoverable")
			return
		}
		credentials := map[string]string{"url": b.cfg.GatewayURL, "api_key": existing.Value.Key, "trusted_ca": b.trustedCA}
		if b.cfg.MCPURL != "" {
			credentials["mcp_url"] = b.cfg.MCPURL
		}
		write(w, 201, map[string]any{"credentials": credentials})
		return
	}
	if r.Method == http.MethodDelete {
		if existing == nil {
			write(w, 410, map[string]any{})
			return
		}
		err = b.gateway(r.Context(), http.MethodDelete, "/"+url.PathEscape(existing.ID), nil, nil)
		var remote *apiError
		if err != nil && !(errors.As(err, &remote) && remote.status == 404) {
			fail(w, 503, "gateway deletion failed; retry the request")
			return
		}
		write(w, 200, map[string]any{})
		return
	}
	fp := fingerprint(req)
	if existing != nil {
		if existing.Value.Metadata[labelPrefix+"fingerprint"] != fp {
			fail(w, 409, "service instance already exists with different provision parameters")
			return
		}
		write(w, 200, map[string]any{})
		return
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		fail(w, 500, "credential generation failed")
		return
	}
	key := Key{Key: "sk-cf-" + base64.RawURLEncoding.EncodeToString(secret), Metadata: map[string]any{
		"name": "cf-" + id, labelPrefix + "instance": id, labelPrefix + "installation": b.cfg.Installation, labelPrefix + "service": req.ServiceID, labelPrefix + "plan": req.PlanID, labelPrefix + "fingerprint": fp, labelPrefix + "org": req.OrganizationGUID, labelPrefix + "space": req.SpaceGUID,
	}, AllowedModels: b.cfg.Models, Budgets: []Budget{{Name: "instance-spend", Limit: Limit{"USD", b.cfg.Budget}, Window: map[string]string{"rolling": b.cfg.Window}, Action: "Block"}}}
	err = b.gateway(r.Context(), http.MethodPut, "", map[string]any{"resources": []any{map[string]any{"value": key}}}, nil)
	if err != nil {
		fail(w, 503, "gateway provisioning failed; retry the request")
		return
	}
	write(w, 201, map[string]any{})
}
func (b *Broker) catalog(w http.ResponseWriter) {
	service := map[string]any{
		"id": b.cfg.ServiceID, "name": "agentgateway", "description": "OpenAI-compatible inference with a shared service-instance spend allowance", "bindable": true, "plan_updateable": false, "instances_retrievable": false, "bindings_retrievable": false,
		"plans": []any{map[string]any{"id": b.cfg.PlanID, "name": "shared", "description": "Shared API key, model allowlist and best-effort USD allowance", "free": true, "metadata": map[string]any{"models": b.cfg.Models, "usd_allowance": b.cfg.Budget, "window": b.cfg.Window, "accounting": "Post-response, epoch-aligned windows; concurrent requests and replica synchronization can overshoot. Unbind does not revoke the shared key."}}},
	}
	for _, backend := range b.cfg.MCPServers {
		if backend.PlanID != "" {
			description := backend.Description
			if strings.TrimSpace(description) == "" {
				description = "Access to the " + backend.Name + " MCP backend"
			}
			service["plans"] = append(service["plans"].([]any), map[string]any{"id": backend.PlanID, "name": backend.Name, "description": description, "free": true, "metadata": map[string]any{"protocol": "mcp", "auth_method": backend.Auth.Method}})
		}
	}
	write(w, 200, map[string]any{"services": []any{service}})
}
func main() {
	path := flag.String("config", "", "JSON configuration file")
	flag.Parse()
	f, err := os.Open(*path)
	if err != nil {
		log.Fatal("cannot open configuration")
	}
	defer f.Close()
	var cfg Config
	dec := json.NewDecoder(f)
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err = dec.Decode(&cfg); err != nil {
		log.Fatal("invalid configuration")
	}
	b, err := newBroker(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: cfg.Listen, Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Print("broker listening")
	if cfg.Certificate != "" {
		err = server.ServeTLS(listener, cfg.Certificate, cfg.PrivateKey)
	} else {
		err = server.Serve(listener)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("broker server failed")
	}
}
