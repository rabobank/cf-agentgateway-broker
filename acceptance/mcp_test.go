package acceptance_test

import (
	"encoding/json"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var _ = Describe("Shared MCP service", func() {
	It("uses broker credentials and commits attributed capsules and a signed checkpoint", func() {
		e := startEnvironment()
		var keys []string
		for _, instance := range []string{"mcp-one", "mcp-two"} {
			url := e.brokerURL() + "/v2/service_instances/" + instance
			expectRequest(201, "PUT", url, fmt.Sprintf(`{"service_id":%q,"plan_id":%q,"organization_guid":"org","space_guid":"space"}`, serviceID, planID), "", true)
			bound := expectRequest(201, "PUT", url+"/service_bindings/app", fmt.Sprintf(`{"service_id":%q,"plan_id":%q}`, serviceID, planID), "", true)
			var binding struct {
				Credentials map[string]string `json:"credentials"`
			}
			Expect(json.Unmarshal(bound.body, &binding)).To(Succeed())
			Expect(binding.Credentials["mcp_url"]).To(Equal(fmt.Sprintf("http://127.0.0.1:%d/mcp", e.gatewayPort)))
			keys = append(keys, binding.Credentials["api_key"])
		}
		call := func(key, session, body string) (string, string, int) {
			GinkgoHelper()
			req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/mcp", e.gatewayPort), strings.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if session != "" {
				req.Header.Set("Mcp-Session-Id", session)
			}
			resp, err := httpClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			return resp.Header.Get("Mcp-Session-Id"), string(raw), resp.StatusCode
		}
		_, _, code := call("invalid", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
		Expect(code).To(Equal(403))
		for _, key := range keys {
			session, body, code := call(key, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
			Expect(code).To(Equal(200), body)
			Expect(session).NotTo(BeEmpty())
			_, body, code = call(key, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
			Expect(code).To(Equal(200), body)
			Expect(body).To(ContainSubstring("capsule works"))
		}
		query := func(sql string) string {
			GinkgoHelper()
			out, err := exec.Command("psql", "-h", "127.0.0.1", "-p", strconv.Itoa(e.postgresPort), "-U", "postgres", "-d", "gateway_acceptance", "-Atc", sql).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "%s", out)
			return strings.TrimSpace(string(out))
		}
		Expect(query("SELECT count(*) FROM agw_capsules WHERE instance_id='mcp-one'")).To(Equal("2"))
		Expect(query("SELECT count(*) FROM agw_capsules WHERE instance_id='mcp-two'")).To(Equal("2"))
		Eventually(func() string {
			return query("SELECT state->'Checkpoint'->>'IndexedSeq' FROM agw_capsule_logs WHERE log_id='acceptance'")
		}).WithTimeout(20 * time.Second).Should(Equal("4"))
		expectRequest(200, "DELETE", e.brokerURL()+"/v2/service_instances/mcp-one?service_id="+serviceID+"&plan_id="+planID, "", "", true)
		_, _, code = call(keys[0], "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		Expect(code).To(Equal(403))
	})
})
