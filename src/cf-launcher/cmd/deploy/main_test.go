package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fakeCF struct {
	calls                           [][]string
	domain                          bool
	management, source, destination string
	policies, existingPolicy        bool
	fail                            string
}

func fixture() *fakeCF {
	return &fakeCF{domain: true, management: "https://management.apps.identity", source: "cf:app:broker-guid", destination: "gateway-guid"}
}

func (f *fakeCF) call(args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.fail != "" && args[0] == f.fail {
		return nil, errors.New("private-upstream-error")
	}
	value := any(map[string]any{})
	if args[0] == "app" {
		if args[1] == "gateway" {
			return []byte("gateway-guid\n"), nil
		}
		return []byte("broker-guid\n"), nil
	}
	if args[0] == "curl" {
		switch {
		case strings.HasPrefix(args[1], "/v3/domains?"):
			value = map[string]any{"resources": []any{map[string]any{"guid": "domain-guid", "enforce_route_policies": f.domain}}}
		case strings.Contains(args[1], "/environment_variables"):
			config, _ := json.Marshal(map[string]any{"gateway": map[string]string{"management_url": f.management}})
			value = map[string]any{"var": map[string]string{"AGENTGATEWAY_CONFIG": string(config), "MANAGEMENT_TOKEN": "secret-marker", "KEEP_ME": "unchanged"}}
		case strings.HasPrefix(args[1], "/v3/routes?"):
			value = map[string]any{"resources": []any{map[string]any{"guid": "route-guid", "destinations": []any{map[string]any{"port": 9090, "app": map[string]string{"guid": f.destination}}}}}}
		case strings.HasPrefix(args[1], "/v3/route_policies?"):
			resources := []any{}
			if f.policies || f.existingPolicy {
				resources = append(resources, map[string]string{"source": f.source})
			}
			value = map[string]any{"resources": resources}
		default:
			return nil, errors.New("unexpected API request")
		}
	}
	return json.Marshal(value)
}

func (f *fakeCF) count(command string) int {
	n := 0
	for _, call := range f.calls {
		if call[0] == command {
			n++
		}
	}
	return n
}

func TestDeploymentOrderingAndManifestVariables(t *testing.T) {
	f := fixture()
	args := []string{"gateway", "broker", "apps.identity", "management", "--vars-file", "/dev/fd/63", "--vars-file", "deployment.yml"}
	if err := deploy(args, f.call); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls[1], []string{"push", "-f", "manifest.yml", "--vars-file", "/dev/fd/63", "--vars-file", "deployment.yml", "--no-start", "--redact-env"}) {
		t.Fatalf("push arguments: %v", f.calls[1])
	}
	if f.count("add-route-policy") != 1 || f.count("unset-env") != 2 {
		t.Fatalf("policy/cleanup: %v", f.calls)
	}
	for _, call := range f.calls {
		if call[0] == "unset-env" && call[2] != "MANAGEMENT_TOKEN" {
			t.Fatalf("unrelated env removed: %v", call)
		}
	}
	want := [][]string{{"restart", "gateway", "--strategy", "rolling"}, {"restart", "broker", "--strategy", "rolling"}}
	if !reflect.DeepEqual(f.calls[len(f.calls)-2:], want) {
		t.Fatalf("restart order: %v", f.calls)
	}
	f = fixture()
	f.existingPolicy = true
	if err := deploy(args[:4], f.call); err != nil || f.count("add-route-policy") != 0 {
		t.Fatalf("idempotent policy: %v", err)
	}
}

func TestUnsafeRoutesAndFailedCommandsNeverRestartApps(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*fakeCF)
	}{
		{"domain", func(f *fakeCF) { f.domain = false }},
		{"caller", func(f *fakeCF) { f.policies = true; f.source = "cf:app:other" }},
		{"destination", func(f *fakeCF) { f.destination = "other" }},
		{"management URL", func(f *fakeCF) { f.management = "https://user:secret-marker@management.apps.identity:444/" }},
		{"failed push", func(f *fakeCF) { f.fail = "push" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixture()
			test.change(f)
			err := deploy([]string{"gateway", "broker", "apps.identity", "management"}, f.call)
			if err == nil || f.count("restart") != 0 || strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "private-upstream-error") {
				t.Fatalf("unsafe failure: %v, %v", err, f.calls)
			}
			if !f.domain && f.count("push") != 0 {
				t.Fatal("pushed before domain verification")
			}
		})
	}
}

func TestStartupAndRouteOverridesRejectedBeforeCF(t *testing.T) {
	for _, flag := range []string{"--no-start", "--no-route", "--random-route", "--no-start=true", "--no-route=true", "--random-route=true"} {
		f := fixture()
		err := deploy([]string{"gateway", "broker", "apps.identity", "management", flag}, f.call)
		if err == nil || len(f.calls) > 0 {
			t.Fatalf("accepted %s", flag)
		}
	}
}

func TestProcessSubstitutionSurvivesLaunchingCF(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.WriteString(`{"broker_config":"private-value"}`); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	path := fmt.Sprintf("/dev/fd/%d", reader.Fd())
	args, cleanup, err := prepareVars([]string{"gateway", "broker", "apps.identity", "management", "--vars-file", path, "--vars-file=deployment.yml"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if args[5] == path || args[6] != "--vars-file=deployment.yml" {
		t.Fatalf("unexpected arguments: %v", args)
	}
	data, err := os.ReadFile(args[5])
	if err != nil || string(data) != `{"broker_config":"private-value"}` {
		t.Fatalf("materialized vars: %v", err)
	}
	info, err := os.Stat(args[5])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("vars file must be private")
	}
	cleanup()
	if _, err := os.Stat(args[5]); !os.IsNotExist(err) {
		t.Fatal("temporary vars not cleaned up")
	}
}
