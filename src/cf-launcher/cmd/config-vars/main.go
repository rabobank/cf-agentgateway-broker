// config-vars converts operator YAML into CF manifest JSON-string variables.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, diagnostics io.Writer) int {
	if len(args) == 3 && args[0] == "--check" {
		broker, err := load(args[1])
		if err != nil {
			return failed(diagnostics)
		}
		gateway, err := load(args[2])
		if err != nil {
			return failed(diagnostics)
		}
		if err := checkReferences(broker, gateway); err != nil {
			fmt.Fprintln(diagnostics, err)
			return 1
		}
		return 0
	}
	if len(args) != 2 || (args[0] != "broker_config" && args[0] != "gateway_config") {
		fmt.Fprintln(diagnostics, "Usage: config-vars broker_config|gateway_config FILE.yml; config-vars --check BROKER.yml GATEWAY.yml")
		return 1
	}
	value, err := load(args[1])
	if err != nil {
		return failed(diagnostics)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return failed(diagnostics)
	}
	if err := json.NewEncoder(out).Encode(map[string]string{args[0]: string(payload)}); err != nil {
		fmt.Fprintln(diagnostics, "Configuration output failed")
		return 1
	}
	return 0
}

func failed(out io.Writer) int {
	// Parser diagnostics may contain credentials or whole source lines.
	fmt.Fprintln(out, "Configuration conversion failed: expected one JSON-compatible YAML object with unique string keys")
	return 1
}

func load(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := yaml.NewDecoder(f)
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("multiple YAML documents")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("object required")
	}
	value, err := jsonValue(doc.Content[0], map[*yaml.Node]bool{})
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}

// Walking nodes keeps YAML-only types, duplicate keys and recursive aliases
// from being silently coerced. Decode scalar numbers directly, never via float64.
func jsonValue(node *yaml.Node, ancestors map[*yaml.Node]bool) (any, error) {
	if ancestors[node] {
		return nil, errors.New("recursive YAML alias")
	}
	ancestors[node] = true
	defer delete(ancestors, node)
	switch node.Kind {
	case yaml.AliasNode:
		return jsonValue(node.Alias, ancestors)
	case yaml.MappingNode:
		result := map[string]any{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return nil, errors.New("string keys required")
			}
			if _, exists := result[key.Value]; exists {
				return nil, errors.New("duplicate key")
			}
			value, err := jsonValue(node.Content[i+1], ancestors)
			if err != nil {
				return nil, err
			}
			result[key.Value] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := jsonValue(child, ancestors)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str":
			return node.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool":
			var value bool
			err := node.Decode(&value)
			return value, err
		case "!!int", "!!float":
			// JSON-compatible decimal syntax avoids precision loss, nonfinite
			// values and implicit reinterpretation of octal/hexadecimal input.
			value := strings.ReplaceAll(node.Value, "_", "")
			if strings.HasPrefix(value, "+") {
				value = value[1:]
			}
			var number json.Number
			if err := json.Unmarshal([]byte(value), &number); err != nil {
				return nil, err
			}
			if node.Tag == "!!float" {
				if _, err := strconv.ParseFloat(value, 64); err != nil {
					return nil, err
				}
			}
			return number, nil
		}
	}
	return nil, errors.New("unsupported YAML value")
}

func object(value any) map[string]any { m, _ := value.(map[string]any); return m }
func list(value any) []any            { a, _ := value.([]any); return a }
func field(value any, path ...string) any {
	for _, key := range path {
		value = object(value)[key]
	}
	return value
}
func contains(values any, item string) bool {
	for _, v := range list(values) {
		if v == item {
			return true
		}
	}
	return false
}

func checkReferences(broker, gateway map[string]any) error {
	models, backends := map[string]bool{}, map[string]any{}
	for _, m := range list(field(gateway, "llm", "models")) {
		name, _ := field(m, "name").(string)
		models[name] = true
	}
	for _, b := range list(field(gateway, "mcp", "backends")) {
		name, _ := field(b, "name").(string)
		backends[name] = b
	}
	if broker["installation"] == nil || broker["installation"] != field(gateway, "capsules", "log_id") {
		return errors.New("broker installation must match capsule log_id")
	}
	for _, service := range list(broker["services"]) {
		for _, plan := range list(field(service, "plans")) {
			for _, name := range list(field(plan, "access", "models")) {
				name, ok := name.(string)
				if !ok || !models[name] {
					return errors.New("plan references unknown model")
				}
			}
			for _, name := range list(field(plan, "access", "backends")) {
				name, ok := name.(string)
				if !ok || backends[name] == nil {
					return errors.New("plan references unknown MCP backend")
				}
				schema := field(plan, "schemas", "service_instance", "create", "parameters")
				for _, target := range list(field(backends[name], "mcp", "targets")) {
					client := field(target, "policies", "backendAuth", "oauthTokenExchange", "clientAuth", "clientId")
					if ref := object(client); ref != nil {
						parameter, ok := ref["fromInstanceParameter"].(string)
						if !ok || field(schema, "properties", parameter) == nil || !contains(field(schema, "required"), parameter) {
							return errors.New("backend identity reference must be a required instance parameter")
						}
					}
				}
			}
		}
	}
	return nil
}
