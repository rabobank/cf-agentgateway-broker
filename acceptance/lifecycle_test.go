package acceptance_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	serviceID = "1b0b97a6-41df-4a39-9b23-53f17a5f85be"
	planID    = "eba8b5b0-e8a7-49cb-8188-cfa52ed43bdd"
)

type response struct {
	status int
	body   []byte
}

var httpClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}

func request(method, url, body, key string, broker bool) (response, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return response{}, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if broker {
		req.SetBasicAuth("broker", brokerPassword)
		req.Header.Set("X-Broker-API-Version", "2.16")
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return response{status: resp.StatusCode, body: data}, err
}

func status(method, url, body, key string, broker bool) int {
	resp, err := request(method, url, body, key, broker)
	if err != nil {
		return 0
	}
	return resp.status
}

func expectRequest(want int, method, url, body, key string, broker bool) response {
	GinkgoHelper()
	resp, err := request(method, url, body, key, broker)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.status).To(Equal(want), "%s %s: %s", method, url, resp.body)
	return resp
}

var _ = Describe("a service instance", func() {
	var env *environment

	BeforeEach(func() {
		env = startEnvironment()
	})

	It("enforces the shared allowance and revokes the instance credential when deprovisioned", func() {
		instance := env.brokerURL() + "/v2/service_instances/acceptance-instance"
		binding := instance + "/service_bindings/acceptance-binding"
		query := "?service_id=" + serviceID + "&plan_id=" + planID
		provision := fmt.Sprintf(`{"service_id":%q,"plan_id":%q,"organization_guid":"acceptance-org","space_guid":"acceptance-space"}`, serviceID, planID)
		bind := fmt.Sprintf(`{"service_id":%q,"plan_id":%q}`, serviceID, planID)
		chatBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello from acceptance"}]}`, model)

		By("provisioning a service instance and binding it")
		expectRequest(http.StatusCreated, http.MethodPut, instance, provision, "", true)
		DeferCleanup(func() {
			_, _ = request(http.MethodDelete, instance+query, "", "", true)
		})
		expectRequest(http.StatusOK, http.MethodPut, instance, provision, "", true)
		bound := expectRequest(http.StatusCreated, http.MethodPut, binding, bind, "", true)
		var result struct {
			Credentials struct {
				URL       string `json:"url"`
				APIKey    string `json:"api_key"`
				TrustedCA string `json:"trusted_ca"`
			} `json:"credentials"`
		}
		Expect(json.Unmarshal(bound.body, &result)).To(Succeed())
		Expect(result.Credentials.URL).To(Equal(fmt.Sprintf("http://127.0.0.1:%d/v1", env.gatewayPort)))
		Expect(result.Credentials.APIKey).To(HavePrefix("sk-cf-"))
		Expect(result.Credentials.TrustedCA).To(BeEmpty())
		key := result.Credentials.APIKey
		modelsURL := result.Credentials.URL + "/models"
		chatURL := result.Credentials.URL + "/chat/completions"
		Eventually(func() int { return status(http.MethodGet, modelsURL, "", key, false) }).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Equal(http.StatusOK))

		By("sending a request through the gateway to the Higress mock")
		completion := expectRequest(http.StatusOK, http.MethodPost, chatURL, chatBody, key, false)
		var answer struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		Expect(json.Unmarshal(completion.body, &answer)).To(Succeed())
		Expect(answer.Choices).To(HaveLen(1))
		Expect(answer.Choices[0].Message.Content).To(Equal("hello from acceptance"))
		Expect(answer.Usage.Prompt).To(Equal(9))
		Expect(answer.Usage.Completion).To(Equal(1))
		Expect(env.providerCalls.Load()).To(Equal(int64(1)))

		By("consuming the remaining allowance")
		expectRequest(http.StatusOK, http.MethodPost, chatURL, chatBody, key, false)
		Expect(env.providerCalls.Load()).To(Equal(int64(2)))

		By("enforcing the USD budget based on provider-reported usage")
		Eventually(func() int { return status(http.MethodPost, chatURL, chatBody, key, false) }).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Equal(http.StatusTooManyRequests))
		callsAfterQuota := env.providerCalls.Load()
		expectRequest(http.StatusTooManyRequests, http.MethodPost, chatURL, chatBody, key, false)
		Expect(env.providerCalls.Load()).To(Equal(callsAfterQuota))

		By("unbinding while preserving the shared instance credential")
		expectRequest(http.StatusOK, http.MethodDelete, binding+query, "", "", true)
		afterUnbind := expectRequest(http.StatusTooManyRequests, http.MethodGet, modelsURL, "", key, false)
		Expect(string(afterUnbind.body)).To(ContainSubstring("budget_exceeded"))

		By("deprovisioning the instance and confirming the credential is revoked")
		expectRequest(http.StatusOK, http.MethodDelete, instance+query, "", "", true)
		Eventually(func() int { return status(http.MethodGet, modelsURL, "", key, false) }).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Equal(http.StatusUnauthorized))
		expectRequest(http.StatusUnauthorized, http.MethodPost, chatURL, chatBody, key, false)
	})
})
