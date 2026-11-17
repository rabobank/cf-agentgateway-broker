package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operator.yml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func invoke(args ...string) (int, string, string) {
	var out, diagnostics bytes.Buffer
	code := run(args, &out, &diagnostics)
	return code, out.String(), diagnostics.String()
}

func TestConversionPreservesJSONTypesAndPEM(t *testing.T) {
	path := configFile(t, "budget: 1.25\nfree: true\nrates: {input: '0.25'}\nca: |\n  PEM\nlarge: 9007199254740993\n")
	for _, key := range []string{"broker_config", "gateway_config"} {
		code, out, diagnostics := invoke(key, path)
		if code != 0 {
			t.Fatalf("conversion: %s", diagnostics)
		}
		var vars map[string]string
		if err := json.Unmarshal([]byte(out), &vars); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		decoder := json.NewDecoder(strings.NewReader(vars[key]))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if value["budget"] != json.Number("1.25") || value["free"] != true || value["ca"] != "PEM\n" || value["large"] != json.Number("9007199254740993") || value["rates"].(map[string]any)["input"] != "0.25" {
			t.Fatalf("values changed: %#v", value)
		}
	}
}

func TestInvalidInputProducesNoPartialOutputOrSecretDiagnostics(t *testing.T) {
	for _, text := range []string{
		"password: secret-marker\npassword: other\n",
		"nested: {password: secret-marker, password: other}\n",
		"x: .nan\n", "x: .inf\n", "x: 1\n---\nx: 2\n",
		"true: secret-marker\n", "password: [secret-marker\n",
		"date: 2026-11-17\n", "x: &loop [*loop]\n", "[]\n", "",
	} {
		t.Run(text, func(t *testing.T) {
			code, out, diagnostics := invoke("broker_config", configFile(t, text))
			if code == 0 || out != "" || diagnostics == "" || strings.Contains(diagnostics, "secret-marker") {
				t.Fatalf("unsafe failure: code=%d stdout=%q stderr=%q", code, out, diagnostics)
			}
		})
	}
	if code, _, _ := invoke("unknown", "missing"); code == 0 {
		t.Fatal("unknown variable accepted")
	}
}

func TestCrossFileReferencesAndInstanceIdentity(t *testing.T) {
	broker := "installation: lab\nservices:\n- protocol: mcp\n  plans:\n  - access: {backends: [tools]}\n    schemas:\n      service_instance:\n        create:\n          parameters:\n            required: [client_id]\n            properties: {client_id: {type: string}}\n"
	gateway := "capsules: {log_id: lab}\nmcp:\n  backends:\n  - name: tools\n    mcp:\n      targets:\n      - name: work\n        policies:\n          backendAuth:\n            oauthTokenExchange:\n              clientAuth:\n                clientId: {fromInstanceParameter: client_id}\n"
	for _, test := range []struct {
		name, broker, gateway, diagnostic string
	}{
		{"valid", broker, gateway, ""},
		{"missing model", "installation: lab\nservices: [{plans: [{access: {models: [missing]}}]}]", gateway, "unknown model"},
		{"missing backend", strings.Replace(broker, "[tools]", "[missing]", 1), gateway, "unknown MCP backend"},
		{"missing identity declaration", strings.Replace(broker, "required: [client_id]", "required: []", 1), gateway, "required instance parameter"},
		{"different installation", broker, strings.Replace(gateway, "log_id: lab", "log_id: other", 1), "log_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, out, diagnostics := invoke("--check", configFile(t, test.broker), configFile(t, test.gateway))
			if out != "" || (code == 0) != (test.diagnostic == "") || !strings.Contains(diagnostics, test.diagnostic) {
				t.Fatalf("check: code=%d stdout=%q stderr=%q", code, out, diagnostics)
			}
		})
	}
}

func TestBundledExamplesHaveValidReferences(t *testing.T) {
	base := filepath.Join("..", "..", "..", "..", "cf")
	if code, _, diagnostics := invoke("--check", filepath.Join(base, "broker.example.yml"), filepath.Join(base, "gateway.example.yml")); code != 0 {
		t.Fatal(diagnostics)
	}
}
