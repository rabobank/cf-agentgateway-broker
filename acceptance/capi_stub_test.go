package acceptance_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

const stubSpace = "team-space"
const mcpOAuthMetadata = "agentgateway_mcp_oauth"

type capiStub struct {
	broker     string
	credential string
	mu         sync.Mutex
	lastPatch  []byte
	lastState  string
}

func startCAPIStub(broker string) (*capiStub, string) {
	// The CF CLI reads the expiry from its cached JWT before sending it. The
	// stub accepts only this exact fixture credential, not arbitrary JWTs.
	enc := base64.RawURLEncoding.EncodeToString
	token := enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d,"sub":"acceptance-cf-user"}`, time.Now().Add(time.Hour).Unix()))) + ".fixture"
	c := &capiStub{broker: broker, lastState: "succeeded", credential: token}
	s := httptest.NewServer(c)
	DeferCleanup(s.Close)
	return c, s.URL
}

func stubJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (c *capiStub) osb(r *http.Request, method, path string, body []byte, out any) int {
	req, err := http.NewRequestWithContext(r.Context(), method, c.broker+"/v2/"+path, bytes.NewReader(body))
	if err != nil {
		return 502
	}
	req.SetBasicAuth("broker", brokerPassword)
	req.Header.Set("X-Broker-API-Version", "2.16")
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 502
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 && out != nil && json.NewDecoder(resp.Body).Decode(out) != nil {
		return 502
	}
	return resp.StatusCode
}

func (c *capiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		root := "http://" + r.Host
		stubJSON(w, 200, map[string]any{"links": map[string]any{"self": map[string]any{"href": root}, "cloud_controller_v3": map[string]any{"href": root + "/v3", "meta": map[string]string{"version": "3.210.0"}}, "uaa": map[string]string{"href": root}, "login": map[string]string{"href": root}}})
		return
	case "/v3/info":
		stubJSON(w, 200, map[string]string{"name": "Acceptance CAPI", "osbapi_version": "2.16"})
		return
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "bearer") || token != c.credential {
		stubJSON(w, 401, map[string]any{"errors": []any{map[string]any{"code": 10002, "title": "CF-NotAuthenticated", "detail": "Authentication required"}}})
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	instance := map[string]any{"guid": "team-instance", "name": "team-design", "type": "managed", "relationships": map[string]any{"service_plan": map[string]any{"data": map[string]string{"guid": "team-plan"}}}, "last_operation": map[string]string{"state": c.lastState, "type": "update"}}
	switch r.Method + " " + r.URL.Path {
	case "GET /v3/service_instances":
		q := r.URL.Query()
		var found []any
		if q.Get("space_guids") == stubSpace && q.Get("type") == "managed" && (q.Get("names") == "" || q.Get("names") == "team-design") {
			found = append(found, instance)
		}
		stubJSON(w, 200, map[string]any{"resources": found, "pagination": map[string]any{"next": nil}})
	case "GET /v3/service_plans/team-plan":
		var catalog struct {
			Services []struct {
				ID    string
				Plans []struct {
					ID       string
					Metadata map[string]any
				}
			}
		}
		if status := c.osb(r, "GET", "catalog", nil, &catalog); status != 200 {
			w.WriteHeader(status)
			return
		}
		for _, s := range catalog.Services {
			for _, p := range s.Plans {
				if p.ID == oauthFixtureName {
					stubJSON(w, 200, map[string]any{"guid": "team-plan", "broker_catalog": map[string]any{"id": p.ID, "metadata": p.Metadata}})
					return
				}
			}
		}
		w.WriteHeader(404)
	case "GET /v3/service_instances/team-instance":
		stubJSON(w, 200, instance)
	case "GET /v3/service_instances/team-instance/parameters":
		var obj struct{ Parameters json.RawMessage }
		if status := c.osb(r, "GET", "service_instances/team-instance", nil, &obj); status != 200 {
			w.WriteHeader(status)
			return
		}
		stubJSON(w, 200, obj.Parameters)
	case "PATCH /v3/service_instances/team-instance":
		var obj struct{ Parameters map[string]json.RawMessage }
		decoder := json.NewDecoder(io.LimitReader(r.Body, 32<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&obj) != nil || len(obj.Parameters) != 1 || obj.Parameters[mcpOAuthMetadata] == nil {
			w.WriteHeader(400)
			return
		}
		body, _ := json.Marshal(map[string]any{"service_id": "team-mcp", "plan_id": oauthFixtureName, "context": map[string]string{"platform": "cloudfoundry", "space_guid": stubSpace}, "parameters": obj.Parameters})
		c.lastPatch = body
		if status := c.osb(r, "PATCH", "service_instances/team-instance?accepts_incomplete=true", body, nil); status != 200 {
			stubJSON(w, 422, map[string]any{"errors": []any{map[string]any{"code": 10008, "title": "CF-UnprocessableEntity", "detail": fmt.Sprintf("Service broker rejected update (%d)", status)}}})
			return
		}
		c.lastState = "succeeded"
		w.Header().Set("Location", "/v3/jobs/update-team-instance")
		w.WriteHeader(202)
	case "GET /v3/jobs/update-team-instance":
		stubJSON(w, 200, map[string]string{"state": "COMPLETE"})
	default:
		fmt.Fprintf(GinkgoWriter, "CAPI fixture: unhandled %s %s\n", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func (c *capiStub) patch() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.lastPatch)
}
