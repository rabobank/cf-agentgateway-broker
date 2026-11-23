package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"llm-mock-server/pkg/provider/chat"
)

const (
	stackImage     = "ghcr.io/cloudfoundry/k8s/cflinuxfs5:latest"
	providerKey    = "acceptance-provider-key"
	brokerPassword = "acceptance-broker-password"
	model          = "acceptance-model"
	managementPort = 9090
)

type environment struct {
	root           string
	work           string
	postgresPort   int
	providerPort   int
	brokerPort     int
	gatewayPort    int
	providerCalls  atomic.Int64
	mcpURL         string
	operatorConfig bool
	sharedMCP      *sharedMCPFixture
	oauthMCP       *oauthPKCEFixture
	containers     map[string]string
}

func startEnvironment() *environment {
	return startConfiguredEnvironment(false)
}

func startConfiguredEnvironment(operator bool) *environment {
	return startEnvironmentWithMCP(operator, false)
}

func startSharedMCPEnvironment() *environment {
	return startEnvironmentWithMCP(true, true)
}

func startEnvironmentWithMCP(operator, shared bool) *environment {
	return startEnvironmentWithFixtures(operator, shared, false)
}

func startEnvironmentWithFixtures(operator, shared, oauth bool) *environment {
	GinkgoHelper()
	root, err := filepath.Abs("..")
	Expect(err).NotTo(HaveOccurred())
	for _, path := range []string{"cf/dist/gateway/bin/agentgateway", "cf/dist/gateway/bin/launcher", "cf/dist/broker/bin/broker", "cf/dist/broker/bin/launcher"} {
		info, err := os.Stat(filepath.Join(root, path))
		Expect(err).NotTo(HaveOccurred(), "build artifacts missing; run devbox run build")
		Expect(info.Mode()&0111).NotTo(BeZero(), "%s must be executable", path)
	}
	work, err := os.MkdirTemp("", "agentgateway-acceptance-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, work)
	e := &environment{root: root, work: work, operatorConfig: operator, containers: map[string]string{}}
	if shared {
		e.sharedMCP = startSharedMCPFixture(work)
	}
	if oauth {
		e.oauthMCP = startOAuthPKCEFixture(work)
	}
	e.startPostgres()
	e.startProvider()
	e.startMCP()
	e.startGateway()
	e.startBroker()
	return e
}

func freePort() int {
	GinkgoHelper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port
	Expect(listener.Close()).To(Succeed())
	return port
}

func run(name string, args ...string) {
	GinkgoHelper()
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "%s %v failed:\n%s", name, args, output)
}

func (e *environment) startPostgres() {
	GinkgoHelper()
	e.postgresPort = freePort()
	data := filepath.Join(e.work, "postgres")
	run("initdb", "-D", data, "-A", "trust", "-U", "postgres")
	DeferCleanup(func() error {
		if err := exec.Command("pg_ctl", "-D", data, "status").Run(); err != nil {
			return nil
		}
		output, err := exec.Command("pg_ctl", "-D", data, "-m", "fast", "-w", "stop").CombinedOutput()
		if err != nil {
			return fmt.Errorf("stop PostgreSQL: %w: %s", err, output)
		}
		return nil
	})
	run("pg_ctl", "-D", data, "-l", filepath.Join(e.work, "postgres.log"), "-o", fmt.Sprintf("-p %d -h 127.0.0.1 -k %s", e.postgresPort, e.work), "-w", "start")
	run("createdb", "-h", "127.0.0.1", "-p", strconv.Itoa(e.postgresPort), "-U", "postgres", "gateway_acceptance")
}

func (e *environment) startProvider() {
	GinkgoHelper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	e.providerPort = listener.Addr().(*net.TCPAddr).Port
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer "+providerKey {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		e.providerCalls.Add(1)
		c.Next()
	})
	chat.SetupRoutes(router, "openai")
	server := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second}
	serveError := make(chan error, 1)
	go func() { serveError <- server.Serve(listener) }()
	DeferCleanup(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			return err
		}
		select {
		case err := <-serveError:
			if err != http.ErrServerClosed {
				return fmt.Errorf("mock provider stopped unexpectedly: %v", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
}

func jsonString(value any) string {
	GinkgoHelper()
	data, err := json.Marshal(value)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func (e *environment) startContainer(role string, env map[string]string) {
	GinkgoHelper()
	name := fmt.Sprintf("agentgateway-acceptance-%s-%d", role, time.Now().UnixNano())
	e.containers[role] = name
	args := []string{"run", "--name", name, "--network", "host", "--tmpfs", "/app:exec,mode=777", "--workdir", "/app"}
	if (e.sharedMCP != nil || e.oauthMCP != nil) && role == "gateway" {
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=/fixture-ca.pem,readonly", filepath.Join(e.work, "fixture-ca.pem")), "--env", "SSL_CERT_FILE=/fixture-ca.pem")
	}
	args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=/app/bin,readonly", filepath.Join(e.root, "cf/dist", role, "bin")))
	if role == "gateway" {
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=/app/lib,readonly", filepath.Join(e.root, "cf/dist/gateway/lib")))
	}
	for key, value := range env {
		args = append(args, "--env", key+"="+value)
	}
	if role == "gateway" {
		command := "./bin/capsule-extmcp & exec ./bin/launcher"
		if e.oauthMCP != nil {
			command = "./bin/capsule-extmcp mcp-oauth & " + command
		}
		if e.sharedMCP != nil {
			command = "test -r /fixture-ca.pem || exit 1; " + command
		}
		args = append(args, stackImage, "sh", "-c", command)
	} else {
		args = append(args, stackImage, "./bin/launcher")
	}
	command := exec.Command("docker", args...)
	session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
		if err != nil && !strings.Contains(string(output), "No such container") {
			return fmt.Errorf("remove container %s: %w: %s", name, err, output)
		}
		if session.Wait(5*time.Second).ExitCode() == -1 {
			session.Kill().Wait(5 * time.Second)
		}
		return nil
	})
}

func (e *environment) startGateway() {
	GinkgoHelper()
	// The CF launcher listens on management port 9090. With host networking it
	// must be available before starting the container.
	management, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", managementPort))
	Expect(err).NotTo(HaveOccurred(), "gateway management port 9090 must be free")
	Expect(management.Close()).To(Succeed())
	e.gatewayPort = freePort()
	uri := fmt.Sprintf("postgres://postgres@127.0.0.1:%d/gateway_acceptance?sslmode=disable", e.postgresPort)
	models := []any{map[string]any{"name": model, "provider": "openAI", "params": map[string]any{"model": model, "apiKey": providerKey, "baseUrl": fmt.Sprintf("http://127.0.0.1:%d/v1", e.providerPort)}}}
	catalog := map[string]any{"providers": map[string]any{"openai": map[string]any{"models": map[string]any{model: map[string]any{"rates": map[string]string{"input": "1000000", "output": "1000000"}}}}}}
	if e.operatorConfig {
		config := map[string]any{"database": map[string]string{"service_name": "acceptance-state"}, "llm": map[string]any{"models": models}, "model_catalog": catalog, "capsules": map[string]string{"log_id": "acceptance", "signing_seed": strings.Repeat("12", 32)}}
		if e.sharedMCP != nil {
			config["mcp"] = e.sharedMCP.gatewayConfig()
		}
		if e.oauthMCP != nil {
			config["mcp"] = e.oauthMCP.gatewayConfig()
		}
		e.startContainer("gateway", map[string]string{"APP_ROLE": "gateway", "PORT": strconv.Itoa(e.gatewayPort), "RUST_MIN_STACK": "16777216", "VCAP_SERVICES": jsonString(map[string]any{"postgres": []any{map[string]any{"name": "acceptance-state", "credentials": map[string]string{"uri": uri}}}}), "AGENTGATEWAY_CONFIG": jsonString(config)})
		Eventually(func() int { return status(http.MethodGet, "http://127.0.0.1:9090/api/runtime", "", "", false) }).WithTimeout(90 * time.Second).Should(Equal(http.StatusOK))
		return
	}
	e.startContainer("gateway", map[string]string{
		"APP_ROLE": "gateway", "PORT": strconv.Itoa(e.gatewayPort),
		"RUST_MIN_STACK": "16777216", "GATEWAY_MODELS": jsonString(models), "MODEL_CATALOG": jsonString(catalog),
		"DATABASE_SERVICE_NAME":      "acceptance-state",
		"MCP_SERVERS":                jsonString([]any{map[string]string{"name": "fixture", "url": e.mcpURL}}),
		"CAPSULE_LOG_ID":             "acceptance",
		"SESSION_KEY":                strings.Repeat("34", 32),
		"CAPSULE_SIGNING_SEED":       strings.Repeat("12", 32),
		"CAPSULE_CHECKPOINT_ENTRIES": "1",
		"VCAP_SERVICES":              jsonString(map[string]any{"postgres": []any{map[string]any{"name": "acceptance-state", "credentials": map[string]string{"uri": uri}}}}),
	})
	Eventually(func() int {
		return status(http.MethodGet, "http://127.0.0.1:9090/api/runtime", "", "", false)
	}).WithTimeout(90*time.Second).WithPolling(250*time.Millisecond).Should(Equal(http.StatusOK), "gateway management did not become ready")
}

func (e *environment) startBroker() {
	GinkgoHelper()
	e.brokerPort = freePort()
	if e.operatorConfig {
		services := []any{map[string]any{"id": serviceID, "name": "inference", "description": "Inference", "protocol": "llm", "connection": map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d/v1", e.gatewayPort)}, "plans": []any{map[string]any{"id": planID, "name": "small", "description": "Allowance", "access": map[string]any{"models": []string{model}}, "budget": map[string]any{"usd": 20, "window": "24h"}}}}}
		if e.sharedMCP != nil {
			services = append(services, sharedMCPOffering(e.gatewayPort))
		}
		config := map[string]any{"installation": "acceptance", "auth": map[string]string{"username": "broker", "password": brokerPassword}, "gateway": map[string]string{"management_url": "http://127.0.0.1:9090"}, "services": services}
		if e.oauthMCP != nil {
			config["team_oauth"] = map[string]string{"binding_key": strings.Repeat("78", 32)}
			config["services"] = append(services, e.oauthMCP.offering(e.gatewayPort))
		}
		e.startContainer("broker", map[string]string{"APP_ROLE": "broker", "PORT": strconv.Itoa(e.brokerPort), "AGENTGATEWAY_CONFIG": jsonString(config)})
		Eventually(func() int { return status(http.MethodGet, e.brokerURL()+"/ready", "", "", false) }).WithTimeout(30 * time.Second).Should(Equal(http.StatusOK))
		return
	}
	e.startContainer("broker", map[string]string{
		"APP_ROLE": "broker", "PORT": strconv.Itoa(e.brokerPort), "BROKER_PASSWORD": brokerPassword,
		"INSTALLATION": "acceptance", "MANAGEMENT_URL": "http://127.0.0.1:9090",
		"GATEWAY_URL":     fmt.Sprintf("http://127.0.0.1:%d/v1", e.gatewayPort),
		"INFERENCE_PLANS": jsonString([]any{map[string]any{"id": "eba8b5b0-e8a7-49cb-8188-cfa52ed43bdd", "name": "acceptance-inference", "description": "Acceptance inference allowance", "free": true, "models": []string{model}, "budget": 20, "window": "24h"}}),
		"MCP_SERVERS":     jsonString([]any{map[string]string{"name": "fixture", "url": e.mcpURL}}),
		"MCP_URL":         fmt.Sprintf("http://127.0.0.1:%d/mcp", e.gatewayPort),
	})
	Eventually(func() int {
		return status(http.MethodGet, e.brokerURL()+"/ready", "", "", false)
	}).WithTimeout(30*time.Second).WithPolling(250*time.Millisecond).Should(Equal(http.StatusOK), "broker did not become ready")
}

func (e *environment) brokerURL() string { return fmt.Sprintf("http://127.0.0.1:%d", e.brokerPort) }

func (e *environment) restartShared(ctx context.Context, role string) {
	GinkgoHelper()
	command := exec.CommandContext(ctx, "docker", "restart", e.containers[role])
	command.Stdout, command.Stderr = GinkgoWriter, GinkgoWriter
	Expect(command.Run()).To(Succeed())
	endpoint := e.brokerURL() + "/ready"
	if role == "gateway" {
		endpoint = "http://127.0.0.1:9090/api/runtime"
	}
	Eventually(func() int { return status("GET", endpoint, "", "", false) }).WithContext(ctx).WithTimeout(60 * time.Second).Should(Equal(200))
}

func (e *environment) startMCP() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "gateway credential leaked upstream", http.StatusForbidden)
			return
		}
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&msg) != nil {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "capsule works"}}}
		default:
			w.WriteHeader(202)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	DeferCleanup(server.Close)
	e.mcpURL = server.URL
}
