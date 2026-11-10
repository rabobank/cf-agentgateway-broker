package integration_test

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/plugin/pluginfakes"
	mcpconfig "cf-mcp-config"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	emit "github.com/action-state-group/capsule-emit/go"
	api "github.com/agentgateway/agentgateway/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Pinned gateway ExtMcp integration")
}

const runtimeImage = "ghcr.io/cloudfoundry/k8s/cflinuxfs5:latest"

type bindingGrant struct {
	mu       sync.Mutex
	active   bool
	identity plugin.Identity
}

func (g *bindingGrant) AuthorizeBinding(ctx context.Context, instance, binding, issuer, subject string) (plugin.Identity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active || instance != g.identity.Instance || binding != g.identity.Binding || issuer != g.identity.Account.Issuer || subject != g.identity.Account.Subject {
		return plugin.Identity{}, fmt.Errorf("binding denied")
	}
	return g.identity, nil
}

var _ = BeforeSuite(func(ctx SpecContext) {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", runtimeImage).Run(); err == nil {
		return
	}
	pull := exec.CommandContext(ctx, "docker", "pull", runtimeImage)
	pull.Stdout, pull.Stderr = GinkgoWriter, GinkgoWriter
	Expect(pull.Run()).To(Succeed(), "prepare runtime image before gateway readiness checks")
}, NodeTimeout(4*time.Minute))

var _ = Describe("Gateway metadata round trip", func() {
	It("validates a caller JWT and exchanges it as a federated client assertion for MCP", func(ctx SpecContext) {
		root, err := filepath.Abs("../../..")
		Expect(err).NotTo(HaveOccurred())
		work, err := os.MkdirTemp("", "federated-mcp-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, work)
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())
		enc := base64.RawURLEncoding.EncodeToString
		jwks := map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}}}
		var mu sync.Mutex
		var exchanged, upstream int
		var assertion string
		keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(jwks)
		}))
		DeferCleanup(keys.Close)
		token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "selected-client" || r.Form.Get("client_assertion") != assertion || r.Form.Get("scope") != "urn:test:resource" {
				http.Error(w, "unexpected exchange", 400)
				return
			}
			exchanged++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"upstream-access","token_type":"Bearer","expires_in":3600}`)
		}))
		DeferCleanup(token.Close)
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer upstream-access" {
				http.Error(w, "wrong upstream credential", 401)
				return
			}
			mu.Lock()
			upstream++
			mu.Unlock()
			var msg struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			json.NewDecoder(r.Body).Decode(&msg)
			if msg.ID == nil {
				w.WriteHeader(202)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			var result any
			switch msg.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]string{"type": "object"}}}}
			default:
				result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "federated-ok"}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}))
		DeferCleanup(backend.Close)
		issuer := "https://issuer.test/oauth/token"
		subject := "cf:service-account:team:worker"
		sign := func(values map[string]any) string {
			claims, _ := json.Marshal(values)
			unsigned := enc([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + enc(claims)
			digest := sha256.Sum256([]byte(unsigned))
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			Expect(err).NotTo(HaveOccurred())
			return unsigned + "." + enc(sig)
		}
		assertion = sign(map[string]any{"iss": issuer, "sub": subject, "aud": "urn:test:exchange", "exp": time.Now().Add(5 * time.Minute).Unix()})
		binding := mcpconfig.Binding{ID: "binding", Instance: mcpconfig.Instance{ID: "instance", Installation: "lab", Parameters: map[string]string{"client_id": "selected-client"}, Backend: mcpconfig.Backend{Name: "tools", URL: backend.URL, Auth: mcpconfig.Auth{Method: "federated_client_credentials", Issuer: issuer, JWKSURL: keys.URL, Audience: "urn:test:exchange", Scopes: []string{"urn:test:resource"}}}}, Account: mcpconfig.Account{Issuer: issuer, Subject: subject}}
		route := binding.Route()
		pol := route["policies"].(map[string]any)
		selected := binding.Selected()
		identity := plugin.Identity{Installation: "lab", Instance: "instance", Binding: "binding", Target: binding.TargetName(), Account: &binding.Account, SelectedIdentity: &selected}
		grant := &bindingGrant{active: true, identity: identity}
		authz := httptest.NewServer(plugin.BindingAuthHandler(grant))
		DeferCleanup(authz.Close)
		pol["extAuthz"].(map[string]any)["host"] = strings.TrimPrefix(authz.URL, "http://")
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		repo := &pluginfakes.FakeRepository{}
		producer := &pluginfakes.FakeProducer{}
		producer.SealReturns(emit.Result{CapsuleID: strings.Repeat("b", 64), Payload: []byte(`{}`), Envelope: []byte("signed")}, nil)
		repo.GetReturns(plugin.Capsule{ID: strings.Repeat("b", 64), Identity: identity, Phase: "planned", Backends: []string{binding.TargetName()}, Tool: "echo"}, nil)
		grpcServer := grpc.NewServer()
		api.RegisterExtMcpServer(grpcServer, plugin.NewServer(repo, producer))
		go grpcServer.Serve(listener)
		DeferCleanup(grpcServer.Stop)
		pol["mcpGuardrails"].(map[string]any)["processors"].([]any)[0].(map[string]any)["host"] = listener.Addr().String()
		target := route["backends"].([]any)[0].(map[string]any)["mcp"].(map[string]any)["targets"].([]any)[0].(map[string]any)
		exchange := target["policies"].(map[string]any)["backendAuth"].(map[string]any)["oauthTokenExchange"].(map[string]any)
		exchange["host"] = strings.TrimPrefix(token.URL, "http://")
		exchange["path"] = "/token"
		delete(exchange, "policies")
		free := func() int {
			l, e := net.Listen("tcp4", "127.0.0.1:0")
			Expect(e).NotTo(HaveOccurred())
			p := l.Addr().(*net.TCPAddr).Port
			l.Close()
			return p
		}
		port := free()
		config := map[string]any{"config": map[string]any{"adminAddr": fmt.Sprintf("127.0.0.1:%d", free()), "readinessAddr": fmt.Sprintf("127.0.0.1:%d", free()), "statsAddr": fmt.Sprintf("127.0.0.1:%d", free()), "session": map[string]string{"key": strings.Repeat("12", 32)}}, "gateways": map[string]any{"inference": map[string]int{"port": port}}, "routes": []any{route}}
		data, err := json.Marshal(config)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(work, "config.json"), data, 0600)).To(Succeed())
		name := fmt.Sprintf("federation-test-%d", time.Now().UnixNano())
		cmd := exec.CommandContext(ctx, "docker", "run", "--pull=never", "--rm", "--name", name, "--network", "host", "-v", filepath.Join(root, "cf/dist/gateway")+":/gateway:ro", "-v", work+":/work:ro", "-e", "LD_LIBRARY_PATH=/gateway/lib", "-e", "RUST_MIN_STACK=16777216", runtimeImage, "/gateway/bin/agentgateway", "-f", "/work/config.json")
		cmd.Stdout, cmd.Stderr = GinkgoWriter, GinkgoWriter
		Expect(cmd.Start()).To(Succeed())
		DeferCleanup(func() { exec.Command("docker", "rm", "-f", name).Run(); cmd.Wait() })
		client := &http.Client{Timeout: 10 * time.Second}
		var session string
		call := func(jwt, body string) (int, string, error) {
			req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d%s", port, binding.Path()), strings.NewReader(body))
			if e != nil {
				return 0, "", e
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer "+jwt)
			if session != "" {
				req.Header.Set("Mcp-Session-Id", session)
			}
			resp, e := client.Do(req)
			if e != nil {
				return 0, "", e
			}
			defer resp.Body.Close()
			if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
				session = id
			}
			raw, e := io.ReadAll(resp.Body)
			return resp.StatusCode, string(raw), e
		}
		initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
		Eventually(func() (int, error) { code, _, e := call(assertion, initialize); return code, e }).WithTimeout(30 * time.Second).Should(Equal(200))
		code, body, err := call(assertion, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(200), body)
		Expect(body).To(ContainSubstring("federated-ok"))
		Expect(repo.SaveCallCount()).To(Equal(2))
		_, plan := repo.SaveArgsForCall(0)
		Expect(plan.Identity.SelectedIdentity).To(Equal(&selected))
		Expect(plan.Identity.Account.Subject).To(Equal(subject))
		_, outcome := repo.SaveArgsForCall(1)
		Expect(outcome.Identity).To(Equal(plan.Identity))
		grant.mu.Lock()
		grant.active = false
		grant.mu.Unlock()
		code, _, err = call(assertion, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(403))
		Expect(repo.SaveCallCount()).To(Equal(2))
		// GET streams and DELETE session requests pass the same live grant gate.
		for _, method := range []string{"GET", "DELETE"} {
			req, e := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, binding.Path()), nil)
			Expect(e).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+assertion)
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Mcp-Session-Id", session)
			resp, e := client.Do(req)
			Expect(e).NotTo(HaveOccurred())
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(403))
		}
		code, _, err = call("invalid", initialize)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(401))
		for _, invalid := range []map[string]any{
			{"iss": issuer, "sub": subject, "aud": "wrong-audience", "exp": time.Now().Add(time.Minute).Unix()},
			{"iss": issuer, "sub": subject, "aud": "urn:test:exchange", "exp": time.Now().Add(-2 * time.Minute).Unix()},
			{"iss": "https://other-issuer.test", "sub": subject, "aud": "urn:test:exchange", "exp": time.Now().Add(time.Minute).Unix()},
		} {
			code, _, err = call(sign(invalid), initialize)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(Equal(401))
		}
		code, _, err = call(sign(map[string]any{"iss": issuer, "sub": "other-account", "aud": "urn:test:exchange", "exp": time.Now().Add(time.Minute).Unix()}), initialize)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(403))
		mu.Lock()
		Expect(exchanged).To(Equal(1))
		Expect(upstream).To(BeNumerically(">=", 2))
		mu.Unlock()
		// A second binding to the same operator backend must not accept the first
		// binding's encrypted session, even when both share one service account.
		other := binding
		other.ID = "other-binding"
		otherRoute := other.Route()
		otherRoute["policies"] = pol
		otherTarget := otherRoute["backends"].([]any)[0].(map[string]any)["mcp"].(map[string]any)["targets"].([]any)[0].(map[string]any)
		otherTarget["policies"] = target["policies"]
		config["routes"] = []any{route, otherRoute}
		data, err = json.Marshal(config)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(work, "config.json"), data, 0600)).To(Succeed())
		grant.mu.Lock()
		grant.active = true
		grant.mu.Unlock()
		// Prove the second route is loaded before checking session replay.
		Eventually(func() (int, error) {
			req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d%s", port, other.Path()), strings.NewReader(initialize))
			if e != nil {
				return 0, e
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer "+assertion)
			resp, e := client.Do(req)
			if e != nil {
				return 0, e
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return resp.StatusCode, nil
		}).WithTimeout(5 * time.Second).Should(Equal(200))
		Eventually(func() (int, error) {
			req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d%s", port, other.Path()), strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`))
			if e != nil {
				return 0, e
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer "+assertion)
			req.Header.Set("Mcp-Session-Id", session)
			resp, e := client.Do(req)
			if e != nil {
				return 0, e
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return resp.StatusCode, nil
		}).WithTimeout(5 * time.Second).Should(Equal(404))
	}, NodeTimeout(90*time.Second))
	It("authenticates and returns the planned capsule ID on the response hook", func(ctx SpecContext) {
		root, err := filepath.Abs("../../..")
		Expect(err).NotTo(HaveOccurred())
		work, err := os.MkdirTemp("", "capsule-gateway-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, work)
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		grpcServer := grpc.NewServer()
		DeferCleanup(grpcServer.Stop)
		repo := &pluginfakes.FakeRepository{}
		producer := &pluginfakes.FakeProducer{}
		identity := plugin.Identity{Installation: "lab", Instance: "one"}
		repo.AuthenticateReturns(identity, nil)
		producer.SealReturns(emit.Result{CapsuleID: strings.Repeat("a", 64), Payload: []byte(`{}`), Envelope: []byte("signed")}, nil)
		repo.GetReturns(plugin.Capsule{ID: strings.Repeat("a", 64), Identity: identity, Phase: "planned", Tool: "echo", Backends: []string{"test"}}, nil)
		api.RegisterExtMcpServer(grpcServer, plugin.NewServer(repo, producer))
		go grpcServer.Serve(listener)
		var mu sync.Mutex
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			var msg struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&msg)
			w.Header().Set("Content-Type", "application/json")
			var result any
			switch msg.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				mu.Lock()
				mu.Unlock()
				result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}}
			default:
				w.WriteHeader(202)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}))
		DeferCleanup(backend.Close)
		free := func() int {
			l, e := net.Listen("tcp4", "127.0.0.1:0")
			Expect(e).NotTo(HaveOccurred())
			p := l.Addr().(*net.TCPAddr).Port
			Expect(l.Close()).To(Succeed())
			return p
		}
		port := free()
		config := map[string]any{"config": map[string]any{"adminAddr": fmt.Sprintf("127.0.0.1:%d", free()), "readinessAddr": fmt.Sprintf("127.0.0.1:%d", free()), "statsAddr": fmt.Sprintf("127.0.0.1:%d", free())}, "mcp": map[string]any{"port": port, "targets": []any{map[string]any{"name": "test", "mcp": map[string]any{"host": backend.URL}}}, "policies": map[string]any{"mcpGuardrails": map[string]any{"processors": []any{map[string]any{"kind": "remote", "host": listener.Addr().String(), "failureMode": "failClosed", "methods": map[string]string{"*": "request", "tools/call": "full"}, "metadata": map[string]string{"capsule_plan": "has(mcpGuardrails.capsule_plan) ? mcpGuardrails.capsule_plan : ''"}, "requestHeaders": map[string]any{"allowed": []string{"authorization"}}}}}}}}
		config["config"].(map[string]any)["session"] = map[string]any{"key": strings.Repeat("34", 32)}
		data, err := json.Marshal(config)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(work, "config.json"), data, 0600)).To(Succeed())
		name := fmt.Sprintf("capsule-test-%d", time.Now().UnixNano())
		cmd := exec.CommandContext(ctx, "docker", "run", "--pull=never", "--rm", "--name", name, "--network", "host", "-v", filepath.Join(root, "cf/dist/gateway")+":/gateway:ro", "-v", work+":/work:ro", "-e", "LD_LIBRARY_PATH=/gateway/lib", runtimeImage, "/gateway/bin/agentgateway", "-f", "/work/config.json")
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		Expect(cmd.Start()).To(Succeed())
		DeferCleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run(); _ = cmd.Wait() })
		client := &http.Client{Timeout: 10 * time.Second}
		var session string
		call := func(body string) (int, string, error) {
			req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), strings.NewReader(body))
			if e != nil {
				return 0, "", e
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer fixture")
			if session != "" {
				req.Header.Set("Mcp-Session-Id", session)
			}
			resp, e := client.Do(req)
			if e != nil {
				return 0, "", e
			}
			defer resp.Body.Close()
			if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
				session = id
			}
			raw, e := io.ReadAll(resp.Body)
			return resp.StatusCode, string(raw), e
		}
		Eventually(func() error {
			_, _, e := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
			return e
		}).WithTimeout(30 * time.Second).Should(Succeed())
		code, body, err := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(200), body)
		Expect(body).To(ContainSubstring("ok"))
		Eventually(repo.SaveCallCount).Should(Equal(2), body)
		_, outcome := repo.SaveArgsForCall(1)
		Expect(outcome.Parent).To(Equal(strings.Repeat("a", 64)))
		original := session
		if session[0] == 'A' {
			session = "B" + session[1:]
		} else {
			session = "A" + session[1:]
		}
		code, body, err = call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusNotFound), body)
		Expect(repo.SaveCallCount()).To(Equal(2))
		session = original
		code, body, err = call(`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), body)
	}, NodeTimeout(90*time.Second))
})
