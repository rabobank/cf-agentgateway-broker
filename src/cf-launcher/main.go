package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func required(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic(name + " is required")
	}
	return v
}
func save(name string, value any) string {
	b, e := json.Marshal(value)
	if e != nil {
		panic(e)
	}
	if e = os.WriteFile(name, b, 0600); e != nil {
		panic(e)
	}
	return name
}
func database(raw, serviceName string) (string, error) {
	var services map[string][]struct {
		Name        string         `json:"name"`
		Credentials map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return "", err
	}
	for _, group := range services {
		for _, s := range group {
			if s.Name == serviceName {
				for _, field := range []string{"uri", "url"} {
					if uri, ok := s.Credentials[field].(string); ok {
						u, e := url.Parse(uri)
						if e != nil || u.Host == "" || !(u.Scheme == "postgres" || u.Scheme == "postgresql") {
							return "", errors.New(fmt.Sprintf("invalid PostgreSQL URI: %v", e))
						}
						return uri, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("%s PostgreSQL binding with uri is required", serviceName)
}
func run(mode string) error {
	port, e := strconv.Atoi(required("PORT"))
	if e != nil || port < 1 || port > 65535 {
		return errors.New("invalid PORT")
	}
	if e = os.MkdirAll("config", 0700); e != nil {
		return e
	}
	ca := os.Getenv("GATEWAY_TRUSTED_CA")
	if e = os.WriteFile("config/inference-ca.pem", []byte(ca), 0600); e != nil {
		return e
	}
	var config any
	binary := "bin/broker"
	args := []string{"broker", "-config", "config/runtime.json"}
	switch mode {
	case "broker":
		var plans []map[string]any
		if err := json.Unmarshal([]byte(required("INFERENCE_PLANS")), &plans); err != nil || plans == nil {
			return errors.New("INFERENCE_PLANS must be a JSON array")
		}
		config = map[string]any{"listen": fmt.Sprintf("0.0.0.0:%d", port), "username": "broker", "password": required("BROKER_PASSWORD"), "installation": required("INSTALLATION"), "gateway_url": required("GATEWAY_URL"), "management_url": required("MANAGEMENT_URL"), "management_token": required("MANAGEMENT_TOKEN"), "service_id": "1b0b97a6-41df-4a39-9b23-53f17a5f85be", "inference_plans": plans}
		mcp, err := mcpConfig(os.Getenv("MCP_SERVERS"))
		if err != nil {
			return err
		}
		if mcp != nil {
			config.(map[string]any)["mcp_url"] = required("MCP_URL")
			var servers []mcpServer
			if err := json.Unmarshal([]byte(os.Getenv("MCP_SERVERS")), &servers); err != nil {
				return err
			}
			config.(map[string]any)["mcp_servers"] = servers
		}
		if ca != "" {
			config.(map[string]any)["trusted_ca_file"] = "config/inference-ca.pem"
		}
	case "gateway":
		serviceName := os.Getenv("DATABASE_SERVICE_NAME")
		if serviceName == "" {
			serviceName = "agentgateway-state"
		}
		uri, e := database(required("VCAP_SERVICES"), serviceName)
		if e != nil {
			return e
		}
		var models any
		if e = json.Unmarshal([]byte(required("GATEWAY_MODELS")), &models); e != nil {
			return errors.New(fmt.Sprintf("invalid GATEWAY_MODELS JSON: %v", e))
		}
		var catalog any
		if e = json.Unmarshal([]byte(required("MODEL_CATALOG")), &catalog); e != nil {
			return errors.New(fmt.Sprintf("invalid MODEL_CATALOG JSON: %v", e))
		}
		path, e := filepath.Abs(save("config/catalog.json", catalog))
		if e != nil {
			return e
		}
		config = map[string]any{
			"config":   map[string]any{"adminAddr": "127.0.0.1:15000", "storage": map[string]any{"mode": "hybrid"}, "database": map[string]any{"url": uri}, "modelCatalog": []any{map[string]any{"file": path}}},
			"gateways": map[string]any{"inference": map[string]any{"port": port}, "management": map[string]any{"port": 9090}},
			"llm":      map[string]any{"gateways": []string{"inference"}, "models": models, "policies": map[string]any{"apiKey": map[string]any{"mode": "strict", "keys": []any{}}, "authorization": map[string]any{"rules": []string{`(request.method == "GET" && request.path == "/v1/models") || (request.method == "POST" && request.path == "/v1/chat/completions")`}}}},
			"ui":       map[string]any{"gateways": []string{"management"}, "policies": map[string]any{"apiKey": map[string]any{"mode": "strict", "keys": []any{map[string]any{"key": required("MANAGEMENT_TOKEN")}}}}},
		}
		if err := configureMCP(config.(map[string]any), os.Getenv("MCP_SERVERS"), os.Getenv("SESSION_KEY")); err != nil {
			return err
		}
		binary = "bin/agentgateway"
		args = []string{"agentgateway", "-f", "config/runtime.json"}
	default:
		return errors.New("APP_ROLE must be broker or gateway")
	}
	save("config/runtime.json", config)
	if e = os.Setenv("LD_LIBRARY_PATH", "lib"); e != nil {
		return e
	}
	return syscall.Exec(binary, args, os.Environ())
}
func main() {
	if e := run(required("APP_ROLE")); e != nil {
		fmt.Printf("failed to launch binary: %v\n", e)
		os.Exit(1)
	}
}
