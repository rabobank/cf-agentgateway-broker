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
	stackImage      = "ghcr.io/cloudfoundry/k8s/cflinuxfs5:latest"
	providerKey     = "acceptance-provider-key"
	managementToken = "acceptance-management-token"
	brokerPassword  = "acceptance-broker-password"
	model           = "acceptance-model"
	managementPort  = 9090
)

type environment struct {
	root          string
	work          string
	postgresPort  int
	providerPort  int
	brokerPort    int
	gatewayPort   int
	providerCalls atomic.Int64
	mcpURL        string
}

func startEnvironment() *environment {
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
	e := &environment{root: root, work: work}
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
	args := []string{"run", "--rm", "--name", name, "--network", "host", "--tmpfs", "/app:exec,mode=777", "--workdir", "/app"}
	args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=/app/bin,readonly", filepath.Join(e.root, "cf/dist", role, "bin")))
	if role == "gateway" {
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=/app/lib,readonly", filepath.Join(e.root, "cf/dist/gateway/lib")))
	}
	for key, value := range env {
		args = append(args, "--env", key+"="+value)
	}
	if role == "gateway" {
		args = append(args, stackImage, "sh", "-c", "./bin/capsule-extmcp & exec ./bin/launcher")
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
	e.startContainer("gateway", map[string]string{
		"APP_ROLE": "gateway", "PORT": strconv.Itoa(e.gatewayPort),
		"MANAGEMENT_TOKEN": managementToken, "GATEWAY_MODELS": jsonString(models), "MODEL_CATALOG": jsonString(catalog),
		"DATABASE_SERVICE_NAME":      "acceptance-state",
		"MCP_SERVERS":                jsonString([]any{map[string]string{"name": "fixture", "url": e.mcpURL}}),
		"CAPSULE_LOG_ID":             "acceptance",
		"SESSION_KEY":                strings.Repeat("34", 32),
		"CAPSULE_SIGNING_SEED":       strings.Repeat("12", 32),
		"CAPSULE_CHECKPOINT_ENTRIES": "1",
		"VCAP_SERVICES":              jsonString(map[string]any{"postgres": []any{map[string]any{"name": "acceptance-state", "credentials": map[string]string{"uri": uri}}}}),
	})
	Eventually(func() int {
		return status(http.MethodGet, "http://127.0.0.1:9090/api/runtime", "", managementToken, false)
	}).WithTimeout(90*time.Second).WithPolling(250*time.Millisecond).Should(Equal(http.StatusOK), "gateway management did not become ready")
}

func (e *environment) startBroker() {
	GinkgoHelper()
	e.brokerPort = freePort()
	e.startContainer("broker", map[string]string{
		"APP_ROLE": "broker", "PORT": strconv.Itoa(e.brokerPort), "BROKER_PASSWORD": brokerPassword,
		"INSTALLATION": "acceptance", "MANAGEMENT_URL": "http://127.0.0.1:9090", "MANAGEMENT_TOKEN": managementToken,
		"GATEWAY_URL": fmt.Sprintf("http://127.0.0.1:%d/v1", e.gatewayPort), "MODEL_ALIASES": model,
		"USD_ALLOWANCE": "20", "BUDGET_WINDOW": "24h",
		"MCP_SERVERS": jsonString([]any{map[string]string{"name": "fixture", "url": e.mcpURL}}),
		"MCP_URL":     fmt.Sprintf("http://127.0.0.1:%d/mcp", e.gatewayPort),
	})
	Eventually(func() int {
		return status(http.MethodGet, e.brokerURL()+"/ready", "", "", false)
	}).WithTimeout(30*time.Second).WithPolling(250*time.Millisecond).Should(Equal(http.StatusOK), "broker did not become ready")
}

func (e *environment) brokerURL() string { return fmt.Sprintf("http://127.0.0.1:%d", e.brokerPort) }

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
