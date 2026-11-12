package main

import (
	mcpconfig "cf-mcp-config"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

func configureMCP(config map[string]any, raw, sessionKey string) error {
	mcp, err := mcpConfig(raw)
	if err != nil {
		return err
	}
	if mcp == nil {
		return nil
	}
	key, err := hex.DecodeString(sessionKey)
	if err != nil || len(key) != 32 {
		return errors.New("SESSION_KEY must be a stable, shared 32-byte hexadecimal key when MCP is enabled")
	}
	config["config"].(map[string]any)["session"] = map[string]any{"key": sessionKey}
	config["mcp"] = mcp
	return nil
}

// The MVP exposes this single operator-owned list to every service instance.
// Upstream authentication may be specified using static HTTP header values.
type mcpServer = mcpconfig.Backend

func mcpConfig(raw string) (map[string]any, error) {
	if raw == "" {
		raw = "[]"
	}
	var servers []mcpServer
	if err := json.Unmarshal([]byte(raw), &servers); err != nil {
		return nil, mcpconfig.JSONError("MCP_SERVERS", err)
	}
	if servers == nil {
		return nil, errors.New("MCP_SERVERS must be a JSON array, not null")
	}
	if len(servers) == 0 {
		return nil, nil
	}
	if err := mcpconfig.Validate(servers); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	targets := []any{}
	for _, server := range servers {
		// Plan-owned backends are exposed only by broker-created binding routes.
		if server.PlanID != "" {
			continue
		}
		u, err := url.Parse(server.URL)
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`).MatchString(server.Name) || seen[server.Name] || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return nil, errors.New("invalid or duplicate MCP server")
		}
		seen[server.Name] = true
		target := map[string]any{"name": server.Name, "mcp": map[string]any{"host": server.URL}}
		headers := map[string]string{}
		for k, v := range server.Headers {
			if strings.ContainsAny(k+v, "\r\n") || http.CanonicalHeaderKey(k) == "" {
				return nil, errors.New("invalid MCP upstream header")
			}
			headers[k] = v
		}
		// A service-instance credential authenticates to this gateway only. Never
		// forward it to a shared upstream; operators supply upstream credentials.
		modifier := map[string]any{"set": headers}
		hasAuthorization := false
		for k := range headers {
			if strings.EqualFold(k, "authorization") {
				hasAuthorization = true
			}
		}
		if !hasAuthorization {
			modifier["remove"] = []string{"authorization"}
		}
		target["policies"] = map[string]any{"requestHeaderModifier": modifier}
		targets = append(targets, target)
	}
	return map[string]any{
		"gateways": []string{"inference"}, "targets": targets,
		"policies": map[string]any{
			"extAuthz": map[string]any{"host": "127.0.0.1:50052", "failureMode": "deny", "protocol": map[string]any{"http": map[string]any{"path": "'/authorize'"}}},
			"mcpGuardrails": map[string]any{"processors": []any{map[string]any{
				"kind": "remote", "host": "127.0.0.1:50051", "failureMode": "failClosed",
				"methods":        map[string]string{"tools/call": "full"},
				"metadata":       map[string]string{"capsule_plan": "has(mcpGuardrails.capsule_plan) ? mcpGuardrails.capsule_plan : ''"},
				"requestHeaders": map[string]any{"allowed": []string{"authorization"}},
			}}},
		},
	}, nil
}
