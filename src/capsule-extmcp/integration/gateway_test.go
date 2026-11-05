package integration_test

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/plugin/pluginfakes"
	"encoding/json"
	"fmt"
	emit "github.com/action-state-group/capsule-emit/go"
	api "github.com/agentgateway/agentgateway/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"io"
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

var _ = BeforeSuite(func(ctx SpecContext) {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", runtimeImage).Run(); err == nil {
		return
	}
	pull := exec.CommandContext(ctx, "docker", "pull", runtimeImage)
	pull.Stdout, pull.Stderr = GinkgoWriter, GinkgoWriter
	Expect(pull.Run()).To(Succeed(), "prepare runtime image before gateway readiness checks")
}, NodeTimeout(4*time.Minute))

var _ = Describe("Gateway metadata round trip", func() {
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
