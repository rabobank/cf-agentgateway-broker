package acceptance_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const sharedIssuer = "https://acceptance.example/workload"
const sharedSubject = "cf:service-account:acceptance:worker"

type sharedMCPFixture struct {
	url, assertion string
	mu             sync.Mutex
	principals     map[string]int
}

func startSharedMCPFixture(work string) *sharedMCPFixture {
	GinkgoHelper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	enc := base64.RawURLEncoding.EncodeToString
	f := &sharedMCPFixture{principals: map[string]int{}}
	claims := jsonString(map[string]any{"iss": sharedIssuer, "sub": sharedSubject, "aud": "exchange", "exp": time.Now().Add(20 * time.Minute).Unix()})
	unsigned := enc([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + enc([]byte(claims))
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	Expect(err).NotTo(HaveOccurred())
	f.assertion = unsigned + "." + enc(sig)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/keys" {
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}}})
			return
		}
		if r.URL.Path == "/token" {
			if r.ParseForm() != nil || r.Form.Get("client_assertion") != f.assertion || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "urn:fixture:tools" {
				http.Error(w, "invalid exchange", 400)
				return
			}
			principal := r.Form.Get("client_id")
			if principal != "principal-one" && principal != "principal-two" {
				http.Error(w, "invalid principal", 400)
				return
			}
			f.mu.Lock()
			f.principals[principal]++
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + principal, "token_type": "Bearer", "expires_in": 3600})
			return
		}
		expected := "principal-one"
		if r.URL.Path == "/two" {
			expected = "principal-two"
		}
		if r.Header.Get("Authorization") != "Bearer access-"+expected {
			http.Error(w, "identity isolation failed", 401)
			return
		}
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&msg) != nil {
			w.WriteHeader(400)
			return
		}
		if msg.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": expected}}}
		default:
			w.WriteHeader(202)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	startFixtureTLS(server, work, key)
	f.url = server.URL
	return f
}

func startFixtureTLS(server *httptest.Server, work string, key *rsa.PrivateKey) {
	GinkgoHelper()
	// Use a real CA-issued server chain. httptest's default certificate is a
	// self-signed leaf, not a CA suitable for the gateway's native root loader.
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Acceptance fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	ca, err := x509.ParseCertificate(caDER)
	Expect(err).NotTo(HaveOccurred())
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leafCert, err := x509.ParseCertificate(leafDER)
	Expect(err).NotTo(HaveOccurred())
	_, err = leafCert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(server.Close)
	// This public CA must be readable by the runtime image's unprivileged user,
	// whose UID differs from the host runner. It contains no private key.
	Expect(os.WriteFile(filepath.Join(work, "fixture-ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644)).To(Succeed())
}

func (f *sharedMCPFixture) gatewayConfig() map[string]any {
	targets := []any{}
	for i, name := range []string{"one", "two"} {
		targets = append(targets, map[string]any{"name": name, "mcp": map[string]string{"host": f.url + "/" + name}, "policies": map[string]any{"backendAuth": map[string]any{"oauthTokenExchange": map[string]any{
			"host": f.url, "path": "/token", "grantType": "federatedClientCredentials", "subjectToken": map[string]string{"tokenType": "urn:ietf:params:oauth:token-type:jwt"}, "scopes": []string{"urn:fixture:tools"}, "clientAuth": map[string]any{"method": "clientSecretPost", "clientId": map[string]string{"fromInstanceParameter": fmt.Sprintf("client_%d", i+1)}},
		}}}})
	}
	return map[string]any{"session_key": strings.Repeat("34", 32), "caller_auth": map[string]any{"issuer": sharedIssuer, "audiences": []string{"exchange"}, "jwks": map[string]string{"url": f.url + "/keys"}}, "backends": []any{map[string]any{"name": "shared", "revision": "v1", "mcp": map[string]any{"targets": targets}}}}
}

func sharedMCPOffering(port int) map[string]any {
	return map[string]any{"id": "shared-mcp", "name": "shared-mcp", "description": "Shared MCP", "protocol": "mcp", "connection": map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d/mcp", port)}, "plans": []any{map[string]any{"id": "shared-tools", "name": "standard", "description": "Tools", "access": map[string]any{"backends": []string{"shared"}}, "schemas": map[string]any{"service_instance": map[string]any{"create": map[string]any{"parameters": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"client_1", "client_2"}, "properties": map[string]any{"client_1": map[string]string{"type": "string"}, "client_2": map[string]string{"type": "string"}}}}}}}}}
}

func (e *environment) sharedCall(endpoint, session, method, tool string) (int, string, string) {
	GinkgoHelper()
	params := any(map[string]any{})
	if method == "initialize" {
		params = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "acceptance", "version": "1"}}
	}
	if method == "tools/call" {
		params = map[string]any{"name": tool, "arguments": map[string]any{}}
	}
	request, err := http.NewRequest("POST", endpoint, strings.NewReader(jsonString(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})))
	Expect(err).NotTo(HaveOccurred())
	request.Header.Set("Authorization", "Bearer "+e.sharedMCP.assertion)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-03-26")
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	response, err := httpClient.Do(request)
	Expect(err).NotTo(HaveOccurred())
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	return response.StatusCode, response.Header.Get("Mcp-Session-Id"), string(body)
}

func (e *environment) queryShared(sql string) string {
	GinkgoHelper()
	command := exec.Command("psql", "-h", "127.0.0.1", "-p", strconv.Itoa(e.postgresPort), "-U", "postgres", "-d", "gateway_acceptance", "-Atc", sql)
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "%s", out)
	return strings.TrimSpace(string(out))
}

var _ = Describe("consolidated shared MCP lifecycle", func() {
	It("persists instance-selected principals and per-target evidence through restarts and revocation", func(ctx SpecContext) {
		e := startSharedMCPEnvironment()
		By("discovering file-owned backend definitions through the hybrid management API")
		definitions := expectRequest(200, "GET", "http://127.0.0.1:9090/api/config/resources/mcp.backend", "", "", false)
		Expect(string(definitions.body)).To(ContainSubstring(`"name":"shared"`))
		path := e.brokerURL() + "/v2/service_instances/shared-instance"
		provision := `{"service_id":"shared-mcp","plan_id":"shared-tools","parameters":{"client_1":"principal-one","client_2":"principal-two"}}`
		bind := jsonString(map[string]any{"service_id": "shared-mcp", "plan_id": "shared-tools", "bind_resource": map[string]string{"app_guid": "app"}, "context": map[string]any{"platform": "cloudfoundry", "service_account": map[string]string{"issuer": sharedIssuer, "subject": sharedSubject}}})
		query := "?service_id=shared-mcp&plan_id=shared-tools"
		By("provisioning with required instance identity and binding against gateway-owned state")
		expectRequest(400, "PUT", path, `{"service_id":"shared-mcp","plan_id":"shared-tools"}`, "", true)
		expectRequest(201, "PUT", path, provision, "", true)
		bound := expectRequest(201, "PUT", path+"/service_bindings/first", bind, "", true)
		var credentials struct {
			Credentials struct {
				URL string `json:"mcp_url"`
			}
		}
		Expect(json.Unmarshal(bound.body, &credentials)).To(Succeed())
		endpoint := credentials.Credentials.URL
		var session string
		Eventually(func() int {
			code, s, _ := e.sharedCall(endpoint, "", "initialize", "")
			if code == 200 {
				session = s
			}
			return code
		}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Equal(200))
		code, _, body := e.sharedCall(endpoint, session, "tools/list", "")
		Expect(code).To(Equal(200), body)
		tools := sharedToolNames(body)
		Expect(tools).To(HaveLen(2), body)
		By("calling both targets and checking principal-specific capsule attribution")
		for _, tool := range tools {
			code, _, body = e.sharedCall(endpoint, session, "tools/call", tool)
			Expect(code).To(Equal(200), body)
			Expect(body).To(ContainSubstring("principal-"))
			Expect(body).NotTo(ContainSubstring(`"isError":true`))
		}
		Eventually(func() string {
			return e.queryShared(`SELECT count(DISTINCT record->'Identity'->'selected_identity'->>'client_id') FROM agw_capsules WHERE instance_id='shared-instance' AND record->>'Phase'='planned'`)
		}).WithContext(ctx).WithTimeout(15 * time.Second).Should(Equal("2"))
		Expect(e.queryShared(`SELECT count(*) FROM agw_capsules WHERE instance_id='shared-instance' AND ((record->'Identity'->>'target' LIKE '%-one' AND record->'Identity'->'selected_identity'->>'client_id'='principal-one') OR (record->'Identity'->>'target' LIKE '%-two' AND record->'Identity'->'selected_identity'->>'client_id'='principal-two'))`)).To(Equal("4"))
		Expect(e.queryShared(`SELECT count(*) FROM agw_config_resources WHERE kind IN ('mcp.instance','mcp.grant') AND deleted_at IS NULL`)).To(Equal("2"))
		By("recovering the same instance and binding after restarting broker and gateway processes")
		e.restartShared(ctx, "broker")
		e.restartShared(ctx, "gateway")
		expectRequest(200, "PUT", path, provision, "", true)
		retry := expectRequest(201, "PUT", path+"/service_bindings/first", bind, "", true)
		Expect(string(retry.body)).To(Equal(string(bound.body)))
		Eventually(func() int {
			code, s, _ := e.sharedCall(endpoint, "", "initialize", "")
			if code == 200 {
				session = s
			}
			return code
		}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Equal(200))
		code, _, body = e.sharedCall(endpoint, session, "tools/call", tools[0])
		Expect(code).To(Equal(200), body)
		By("revoking an existing session on unbind and creating an isolated new binding")
		expectRequest(200, "DELETE", path+"/service_bindings/first"+query, "", "", true)
		code, _, _ = e.sharedCall(endpoint, session, "tools/list", "")
		Expect(code).NotTo(Equal(200))
		second := expectRequest(201, "PUT", path+"/service_bindings/second", bind, "", true)
		Expect(json.Unmarshal(second.body, &credentials)).To(Succeed())
		endpoint = credentials.Credentials.URL
		// A new binding must not accept the previous binding's session identity.
		code, _, _ = e.sharedCall(endpoint, session, "tools/list", "")
		Expect(code).NotTo(Equal(200))
		Eventually(func() int {
			code, s, _ := e.sharedCall(endpoint, "", "initialize", "")
			if code == 200 {
				session = s
			}
			return code
		}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Equal(200))
		By("revoking all child access when the parent is deprovisioned")
		expectRequest(200, "DELETE", path+query, "", "", true)
		code, _, _ = e.sharedCall(endpoint, session, "tools/list", "")
		Expect(code).NotTo(Equal(200))
		Expect(e.queryShared(`SELECT count(*) FROM agw_config_resources WHERE kind IN ('mcp.instance','mcp.grant') AND deleted_at IS NULL`)).To(Equal("0"))
	}, SpecTimeout(5*time.Minute))
})

func sharedToolNames(body string) []string {
	GinkgoHelper()
	// Streamable HTTP can return JSON or SSE data records.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var message struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &message) == nil && len(message.Result.Tools) > 0 {
			names := []string{}
			for _, tool := range message.Result.Tools {
				names = append(names, tool.Name)
			}
			return names
		}
	}
	return nil
}
