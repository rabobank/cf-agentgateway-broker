package plugin_test

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/plugin/pluginfakes"
	"errors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"net/http"
	"net/http/httptest"
)

var _ = Describe("HTTP MCP authentication", func() {
	It("authenticates every HTTP method before MCP session handling", func() {
		repo := &pluginfakes.FakeRepository{}
		handler := plugin.AuthHandler(repo)
		req := httptest.NewRequest(http.MethodGet, "http://localhost/authorize", nil)
		req.Header.Set("Authorization", "Bearer shared-instance-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		Expect(response.Code).To(Equal(http.StatusOK))
		_, token := repo.AuthenticateArgsForCall(0)
		Expect(token).To(Equal("shared-instance-key"))
		repo.AuthenticateReturns(plugin.Identity{}, errors.New("revoked"))
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		Expect(response.Code).To(Equal(http.StatusForbidden))
	})
})
