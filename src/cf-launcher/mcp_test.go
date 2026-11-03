package main

import (
	"encoding/json"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestMCPConfiguration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Global MCP configuration")
}

var _ = Describe("Operator-configured MCP servers", func() {
	It("configures one global target list and fail-closed loopback recording", func() {
		config, err := mcpConfig(`[ {"name":"shared","url":"https://tools.example/mcp"} ]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(config["gateways"]).To(Equal([]string{"inference"}))
		data, err := json.Marshal(config)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("https://tools.example/mcp"))
		Expect(string(data)).To(ContainSubstring("127.0.0.1:50051"))
		Expect(string(data)).To(ContainSubstring("mcpGuardrails.capsule_plan"))
		Expect(string(data)).To(ContainSubstring("failClosed"))
	})
	It("rejects malformed, duplicate or non-HTTP targets", func() {
		for _, raw := range []string{`{}`, `[{"name":"a","url":"file:///etc/passwd"}]`, `[{"name":"a","url":"https://tools/mcp"},{"name":"a","url":"https://other/mcp"}]`} {
			_, err := mcpConfig(raw)
			Expect(err).To(HaveOccurred())
		}
	})
	It("leaves MCP disabled for an empty list", func() { config, err := mcpConfig(`[]`); Expect(err).NotTo(HaveOccurred()); Expect(config).To(BeNil()) })
})
