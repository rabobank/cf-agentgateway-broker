package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"code.cloudfoundry.org/cli/v8/plugin"
	models "code.cloudfoundry.org/cli/v8/plugin/models"
)

type patchCLI struct {
	plugin.CliConnection
	endpoint string
	skipTLS  bool
}

func (c patchCLI) ApiEndpoint() (string, error) { return c.endpoint, nil }
func (c patchCLI) AccessToken() (string, error) { return "Bearer fixture", nil }
func (c patchCLI) IsSSLDisabled() (bool, error) { return c.skipTLS, nil }

func TestPatchHonorsCFTLSSettingWithoutChangingOAuthClient(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("expected an authenticated CAPI PATCH")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer s.Close()
	c := patchCLI{endpoint: s.URL}
	if err := patchInstance(c, "/v3/service_instances/instance", []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "TLS verification") {
		t.Fatalf("untrusted certificate should fail by default: %v", err)
	}
	c.skipTLS = true
	if err := patchInstance(c, "/v3/service_instances/instance", []byte(`{}`)); err != nil {
		t.Fatalf("explicit CF TLS setting should allow the update: %v", err)
	}
	if resp, err := oauthHTTP.Get(s.URL); err == nil {
		resp.Body.Close()
		t.Fatal("OAuth client stopped verifying certificates")
	}
}

type instanceLookupCLI struct {
	plugin.CliConnection
	name, result string
	wrongName    bool
}

func (c instanceLookupCLI) GetCurrentSpace() (models.Space, error) {
	var space models.Space
	space.Guid = "space"
	return space, nil
}

func (c *instanceLookupCLI) CliCommandWithoutTerminalOutput(args ...string) ([]string, error) {
	if c.result != "" {
		panic("lookup must not proceed to plan discovery")
	}
	c.result = args[1]
	u, _ := url.Parse(args[1])
	var instances []instance
	// Mirror CAPI: split after query decoding, then URI-unescape each element.
	for _, value := range strings.Split(u.Query().Get("names"), ",") {
		name, _ := url.PathUnescape(value)
		if name == c.name || name == "design" {
			if c.wrongName {
				name = "design"
			}
			instances = append(instances, instance{Name: name, GUID: "instance"})
		}
	}
	data, _ := json.Marshal(map[string]any{"resources": instances})
	return []string{string(data)}, nil
}

func TestInstanceLookupPreservesLiteralFilterElements(t *testing.T) {
	const name = "design, team%2C+"
	cf := &instanceLookupCLI{name: name}
	// The matching fixture has no plan GUID, so execution stops just after name selection.
	if err := run(cf, []string{"disconnect", name}); err == nil || err.Error() != "invalid CF instance or plan GUID" {
		t.Fatalf("expected the exact instance to be selected; got %v", err)
	}
	cf = &instanceLookupCLI{name: name, wrongName: true}
	if err := run(cf, []string{"disconnect", name}); err == nil || !strings.Contains(err.Error(), "exactly matching") {
		t.Fatalf("expected a mismatched response to be rejected before plan discovery; got %v", err)
	}
}
