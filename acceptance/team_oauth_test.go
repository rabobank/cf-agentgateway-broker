package acceptance_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("team MCP OAuth authorization code with PKCE", Label("cf-plugin"), func() {
	for _, mode := range []string{"static loopback", "dynamic pasted callback"} {
		It("connects through real cf ag and CAPI, gates tools, and fences stale updates with "+mode, func(ctx SpecContext) {
			cf, err := exec.LookPath("cf")
			if err != nil && os.Getenv("REQUIRE_CF_CLI") != "true" {
				Skip("install CF CLI v8 to run the plugin acceptance spec")
			}
			Expect(err).NotTo(HaveOccurred())
			dynamic := mode == "dynamic pasted callback"
			e := startEnvironmentWithFixtures(true, false, true, dynamic)
			stub, capi := startCAPIStub(e.brokerURL())
			home := filepath.Join(e.work, "cli-home")
			Expect(os.MkdirAll(filepath.Join(home, ".cf"), 0700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(home, ".cf/config.json"), []byte(jsonString(map[string]any{"ConfigVersion": 4, "Target": capi, "UaaEndpoint": capi, "AuthorizationEndpoint": capi, "APIVersion": "3.210.0", "AccessToken": "bearer " + stub.credential, "OrganizationFields": map[string]string{"GUID": "team-org", "Name": "team-org"}, "SpaceFields": map[string]string{"GUID": stubSpace, "Name": stubSpace}})), 0600)).To(Succeed())
			browserDir := filepath.Join(e.work, "browser")
			Expect(os.Mkdir(browserDir, 0700)).To(Succeed())
			exe, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(browserDir, "xdg-open"), []byte("#!/bin/sh\nAG_ACCEPTANCE_BROWSER_CA=\"$SSL_CERT_FILE\" exec \"$AG_ACCEPTANCE_TEST_BINARY\" \"$@\"\n"), 0700)).To(Succeed())
			cli := func(args ...string) string {
				GinkgoHelper()
				cmd := exec.CommandContext(ctx, cf, args...)
				cmd.Env = append(os.Environ(), "CF_HOME="+home, "CF_TRACE=false", "PATH="+browserDir+":"+os.Getenv("PATH"), "SSL_CERT_FILE="+filepath.Join(e.work, "fixture-ca.pem"), "AG_ACCEPTANCE_TEST_BINARY="+exe)
				var out []byte
				if dynamic && len(args) >= 2 && args[1] == "connect" {
					cmd.Args = append(cmd.Args, "--paste")
					out, err = drivePastedConsent(cmd, e.oauthMCP.server.Client())
				} else {
					out, err = cmd.CombinedOutput()
				}
				Expect(err).NotTo(HaveOccurred(), "%s: %s", args, out)
				Expect(string(out)).NotTo(ContainSubstring(fixtureOAuthAccessToken))
				Expect(string(out)).NotTo(ContainSubstring(fixtureClientSecret))
				return string(out)
			}
			cli("install-plugin", "-f", filepath.Join(e.root, "cf/dist/cf-agentgateway"))
			path := e.brokerURL() + "/v2/service_instances/team-instance"
			provision := jsonString(map[string]string{"service_id": "team-mcp", "plan_id": oauthFixtureName, "space_guid": stubSpace})
			created := expectRequest(201, "PUT", path, provision, "", true)
			Expect(string(created.body)).NotTo(ContainSubstring("dashboard_url"))
			Expect(status("GET", fmt.Sprintf("http://127.0.0.1:%d/internal/mcp-oauth/team-instance", e.gatewayPort), "", "", false)).To(BeElementOf(401, 403, 404))
			Expect(status("GET", capi+"/v3/service_instances", "", "", false)).To(Equal(401))
			bind := jsonString(map[string]any{"service_id": "team-mcp", "plan_id": oauthFixtureName, "context": map[string]string{"platform": "cloudfoundry"}, "bind_resource": map[string]string{"app_guid": "team-agent"}})
			bound := expectRequest(201, "PUT", path+"/service_bindings/agent", bind, "", true)
			var result struct {
				Credentials struct {
					URL     string `json:"mcp_url"`
					Headers map[string]string
				}
			}
			Expect(json.Unmarshal(bound.body, &result)).To(Succeed())
			endpoint, bearer := result.Credentials.URL, result.Credentials.Headers["Authorization"]
			Expect(bearer).To(HavePrefix("Bearer "))
			call := func(session, method, tool string) (int, string, string) {
				GinkgoHelper()
				params := map[string]any{}
				if method == "initialize" {
					params = map[string]any{"protocolVersion": "2025-03-26", "clientInfo": map[string]string{"name": "acceptance", "version": "1"}, "capabilities": map[string]any{}}
				}
				if method == "tools/call" {
					params = map[string]any{"name": tool, "arguments": map[string]any{}}
				}
				req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(jsonString(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})))
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Authorization", bearer)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				if session != "" {
					req.Header.Set("Mcp-Session-Id", session)
				}
				resp, err := httpClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				Expect(err).NotTo(HaveOccurred())
				return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), string(body)
			}
			By("discovering the public OAuth profile and connecting without a dashboard")
			Expect(cli("agentgateway", "list")).To(ContainSubstring("team-design: not connected"))
			Expect(cli("ag", "connect", "team-design")).To(ContainSubstring("team-design: connected"))
			Expect(e.oauthMCP.exchanges.Load()).To(Equal(int64(1)))
			connect := stub.patch()
			Expect(connect).NotTo(ContainSubstring(`"plan_id"`))
			var explicitPlan map[string]any
			Expect(json.Unmarshal([]byte(connect), &explicitPlan)).To(Succeed())
			explicitPlan["plan_id"] = "different-plan"
			expectRequest(409, "PATCH", path, jsonString(explicitPlan), "", true)
			delete(explicitPlan, "plan_id")
			update := explicitPlan["parameters"].(map[string]any)[mcpOAuthMetadata].(map[string]any)
			update["access"].(map[string]any)["token"] = "different-token-for-the-same-operation"
			expectRequest(409, "PATCH", path, jsonString(explicitPlan), "", true)
			Expect(connect).To(ContainSubstring(fixtureAccessToken(1)))
			status := expectRequest(200, "GET", path, "", "", true)
			Expect(string(status.body)).NotTo(ContainSubstring(fixtureOAuthAccessToken))
			Expect(string(status.body)).NotTo(ContainSubstring(fixtureRefreshToken))
			Expect(string(status.body)).NotTo(ContainSubstring(fixtureClientSecret))
			findSession := func() string {
				var session string
				Eventually(func() (int, error) {
					code, s, body := call("", "initialize", "")
					session = s
					if code == 200 && !strings.Contains(body, "serverInfo") {
						return code, fmt.Errorf("invalid initialization")
					}
					return code, nil
				}).WithContext(ctx).WithTimeout(20 * time.Second).Should(Equal(200))
				Expect(session).NotTo(BeEmpty())
				return session
			}
			session := findSession()
			code, _, body := call(session, "tools/list", "")
			Expect(code).To(Equal(200), body)
			Expect(sharedToolNames(body)).To(HaveLen(1))
			tool := sharedToolNames(body)[0]
			Expect(tool).To(HaveSuffix("read_design"))
			_, _, body = call(session, "tools/call", strings.TrimSuffix(tool, "read_design")+"write_design")
			Expect(body).NotTo(ContainSubstring("upstream-ok"))
			Expect(e.oauthMCP.writes.Load()).To(BeZero())
			_, _, body = call(session, "tools/call", tool)
			Expect(body).To(ContainSubstring("upstream-ok"))
			By("recovering the same update and binding after a restart")
			e.restartShared(ctx, "broker")
			e.restartShared(ctx, "gateway")
			expectRequest(200, "PATCH", path, connect, "", true)
			Expect(cli("ag", "status", "team-design")).To(ContainSubstring("team-design: connected"))
			session = findSession()
			code, _, body = call(session, "tools/call", tool)
			Expect(code).To(Equal(200), body)
			Expect(body).To(ContainSubstring("upstream-ok: authorization 1"))
			if dynamic {
				By("refreshing on upstream rejection without replaying tool calls, including after restart")
				Expect(cli("ag", "status", "team-design")).To(ContainSubstring("automatic refresh enabled"))
				for n := int64(1); n <= 2; n++ {
					e.oauthMCP.expireNext.Store(true)
					_, _, body = call(session, "tools/call", tool)
					Expect(body).NotTo(ContainSubstring("upstream-ok"))
					Expect(e.oauthMCP.refreshes.Load()).To(Equal(n))
					expectRequest(200, "PATCH", path, connect, "", true)
					code, _, body = call(session, "tools/call", tool)
					Expect(code).To(Equal(200), body)
					Expect(body).To(ContainSubstring("upstream-ok: authorization 1"))
					Expect(body).To(ContainSubstring("refreshed access token"))
				}
			}
			By("disconnecting and blocking old sessions and replayed connect updates")
			Expect(cli("ag", "disconnect", "team-design")).To(ContainSubstring("team-design: not connected"))
			expectRequest(409, "PATCH", path, connect, "", true)
			code, _, body = call(session, "tools/call", tool)
			Expect(code).NotTo(Equal(200), body)
			By("keeping old sessions invalid even if a reconnect reuses the operation ID and token")
			var reconnect struct {
				ServiceID  string                    `json:"service_id"`
				Parameters map[string]map[string]any `json:"parameters"`
			}
			Expect(json.Unmarshal([]byte(connect), &reconnect)).To(Succeed())
			var disconnected struct {
				Parameters map[string]struct {
					Operation string `json:"operation"`
					Sequence  uint64 `json:"sequence"`
				} `json:"parameters"`
			}
			Expect(json.Unmarshal(expectRequest(200, "GET", path, "", "", true).body, &disconnected)).To(Succeed())
			previous := disconnected.Parameters[mcpOAuthMetadata]
			reconnect.Parameters[mcpOAuthMetadata]["previous"] = previous.Operation
			reconnect.Parameters[mcpOAuthMetadata]["sequence"] = previous.Sequence
			expectRequest(200, "PATCH", path, jsonString(reconnect), "", true)
			_, _, body = call(session, "tools/call", tool)
			Expect(body).NotTo(ContainSubstring("upstream-ok"))
			code, _, body = call(findSession(), "tools/call", tool)
			Expect(code).To(Equal(200), body)
			Expect(body).To(ContainSubstring("upstream-ok: authorization 1"))
			Expect(cli("ag", "disconnect", "team-design")).To(ContainSubstring("team-design: not connected"))
			Expect(cli("ag", "connect", "team-design")).To(ContainSubstring("team-design: connected"))
			By("replacing the upstream authorization and invalidating its existing session")
			session = findSession()
			code, _, body = call(session, "tools/call", tool)
			Expect(code).To(Equal(200), body)
			Expect(body).To(ContainSubstring("upstream-ok: authorization 2"))
			Expect(cli("ag", "connect", "team-design", "--replace")).To(ContainSubstring("team-design: connected"))
			Expect(e.oauthMCP.exchanges.Load()).To(Equal(int64(3)))
			if dynamic {
				Expect(e.oauthMCP.registrations.Load()).To(Equal(int64(3)))
			}
			_, _, body = call(session, "tools/call", tool)
			Expect(body).NotTo(ContainSubstring("upstream-ok"))
			session = findSession()
			code, _, body = call(session, "tools/call", tool)
			Expect(code).To(Equal(200), body)
			Expect(body).To(ContainSubstring("upstream-ok: authorization 3"))
			expectRequest(200, "DELETE", path+"/service_bindings/orphan?service_id=team-mcp&plan_id="+oauthFixtureName, "", "", true)
			expectRequest(200, "DELETE", path+"/service_bindings/agent?service_id=team-mcp&plan_id="+oauthFixtureName, "", "", true)
			code, _, body = call(session, "tools/call", tool)
			Expect(code).NotTo(Equal(200), body)
			expectRequest(200, "DELETE", path+"?service_id=team-mcp&plan_id="+oauthFixtureName, "", "", true)
		})
	}
})

func drivePastedConsent(cmd *exec.Cmd, browser *http.Client) ([]byte, error) {
	tty, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReader(tty)
	var output strings.Builder
	for {
		line, err := reader.ReadString('\n')
		output.WriteString(line)
		if err != nil {
			return []byte(output.String()), fmt.Errorf("missing OAuth consent URL: %w", err)
		}
		if !strings.HasPrefix(line, "https://") {
			continue
		}
		browser := *browser
		browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := browser.Get(strings.TrimSpace(line))
		if err != nil {
			return []byte(output.String()), fmt.Errorf("fixture consent: %w", err)
		}
		resp.Body.Close()
		callback := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusFound || callback == "" {
			return []byte(output.String()), fmt.Errorf("fixture consent status: %d", resp.StatusCode)
		}
		bad, err := url.Parse(callback)
		if err != nil {
			return nil, err
		}
		q := bad.Query()
		q.Set("state", "wrong-state")
		bad.RawQuery = q.Encode()
		if _, err := fmt.Fprintln(tty, bad); err != nil {
			return nil, err
		}
		for !strings.Contains(output.String(), "Invalid OAuth callback") {
			line, err := reader.ReadString('\n')
			output.WriteString(line)
			if err != nil {
				return []byte(output.String()), fmt.Errorf("invalid callback was not rejected: %w", err)
			}
		}
		if _, err := fmt.Fprintln(tty, callback); err != nil {
			return nil, err
		}
		_, _ = io.Copy(&output, reader)
		return []byte(output.String()), cmd.Wait()
	}
}
