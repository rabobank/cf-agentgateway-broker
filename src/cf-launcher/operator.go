package main

import (
	mcpconfig "cf-mcp-config"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func operatorGateway(raw []byte, port int) (map[string]any, error) {
	var c mcpconfig.GatewayOperator
	if err := mcpconfig.DecodeOperator(raw, &c); err != nil {
		return nil, err
	}
	if c.Database.ServiceName == "" {
		return nil, errors.New("database.service_name required")
	}
	uri, err := database(os.Getenv("VCAP_SERVICES"), c.Database.ServiceName)
	if err != nil {
		return nil, err
	}
	config := map[string]any{"config": map[string]any{"adminAddr": "127.0.0.1:15000", "storage": map[string]any{"mode": "hybrid"}, "database": map[string]any{"url": uri}}, "gateways": map[string]any{"inference": map[string]any{"port": port}, "management": map[string]any{"port": 9090}}, "ui": map[string]any{"gateways": []string{"management"}}}
	if len(c.LLM.Models) > 0 {
		if c.LLM.Providers == nil {
			c.LLM.Providers = []json.RawMessage{}
		}
		config["llm"] = map[string]any{"gateways": []string{"inference"}, "providers": c.LLM.Providers, "models": c.LLM.Models, "policies": map[string]any{"apiKey": map[string]any{"mode": "strict", "keys": []any{}}, "authorization": map[string]any{"rules": []string{`(request.method == "GET" && request.path == "/v1/models") || (request.method == "POST" && request.path == "/v1/chat/completions")`}}}}
	}
	if len(c.ModelCatalog) > 0 {
		config["config"].(map[string]any)["modelCatalog"] = []any{map[string]any{"inline": c.ModelCatalog}}
	}
	if len(c.MCP.Backends) > 0 {
		key, e := hex.DecodeString(c.MCP.SessionKey)
		if e != nil || len(key) != 32 {
			return nil, errors.New("mcp.session_key must be 32-byte hexadecimal")
		}
		if c.MCP.OAuthDashboard != nil {
			if err := c.MCP.OAuthDashboard.Validate(); err != nil {
				return nil, err
			}
			public := strings.TrimRight(c.MCP.OAuthDashboard.PublicURL, "/")
			u, _ := url.Parse(public)
			config["routes"] = []any{map[string]any{
				"name": "mcp-oauth-dashboard", "gateways": []string{"inference"}, "hostnames": []string{u.Hostname()},
				"matches":  []any{map[string]any{"path": map[string]any{"pathPrefix": "/mcp-connections/"}}},
				"backends": []any{map[string]any{"host": "127.0.0.1:50053"}},
			}}
		}
		if c.MCP.OAuthUpdates != nil {
			if err := c.MCP.OAuthUpdates.Validate(); err != nil {
				return nil, err
			}
			if c.MCP.OAuthDashboard != nil {
				return nil, errors.New("configure only one team OAuth connection interface")
			}
			config["routes"] = []any{map[string]any{
				"name": "mcp-oauth-management", "gateways": []string{"management"},
				"matches":  []any{map[string]any{"path": map[string]any{"pathPrefix": "/internal/mcp-oauth/"}}},
				"backends": []any{map[string]any{"host": "127.0.0.1:50053"}},
			}}
		}
		jwtBackends := false
		for _, backend := range c.MCP.Backends {
			jwtBackends = jwtBackends || backend.TeamOAuth == nil
		}
		if jwtBackends {
			if c.MCP.CallerAuth.Issuer == "" || len(c.MCP.CallerAuth.Audiences) != 1 || c.MCP.CallerAuth.JWKS.URL == "" {
				return nil, errors.New("mcp.caller_auth requires issuer, one audience and jwks.url")
			}
			for _, rawURL := range []string{c.MCP.CallerAuth.Issuer, c.MCP.CallerAuth.JWKS.URL} {
				u, e := url.Parse(rawURL)
				if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
					return nil, errors.New("MCP caller issuer and JWKS must be HTTPS URLs without credentials, query or fragment")
				}
			}
		}
		config["config"].(map[string]any)["session"] = map[string]any{"key": c.MCP.SessionKey}
		config["mcp"] = map[string]any{"gateways": []string{"inference"}, "targets": []any{}}
		definitions := []any{}
		seen := map[string]bool{}
		for _, b := range c.MCP.Backends {
			if !mcpconfig.ValidID(b.Name) || !mcpconfig.ValidID(b.Revision) || seen[b.Name] || len(b.MCP.Targets) == 0 {
				return nil, errors.New("MCP backend requires unique name, revision and targets")
			}
			seen[b.Name] = true
			if b.TeamOAuth != nil {
				if (c.MCP.OAuthDashboard == nil && c.MCP.OAuthUpdates == nil) || len(b.MCP.Targets) != 1 {
					return nil, errors.New("team OAuth requires an enabled connection interface and one target")
				}
				if err := b.TeamOAuth.Validate(); err != nil {
					return nil, err
				}
			}
			names := map[string]bool{}
			oauthResource := ""
			for _, target := range b.MCP.Targets {
				if err := validateIdentityMarkers(target, ""); err != nil {
					return nil, err
				}
				name, _ := target["name"].(string)
				if !mcpconfig.ValidID(name) || names[name] {
					return nil, errors.New("MCP targets require unique identifiers")
				}
				mcp, ok := target["mcp"].(map[string]any)
				if !ok {
					return nil, errors.New("MCP targets require native mcp configuration")
				}
				host, ok := mcp["host"].(string)
				if !ok {
					return nil, errors.New("MCP targets require host")
				}
				u, e := url.Parse(host)
				if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
					return nil, errors.New("MCP targets require HTTPS endpoint without credentials or fragment")
				}
				names[name] = true
				if b.TeamOAuth != nil {
					if len(target) != 2 || len(mcp) != 1 || !mcpconfig.OAuthURL(host) {
						return nil, errors.New("team OAuth targets accept only name and an HTTPS mcp.host")
					}
					oauthResource = host
					target["mcp"] = map[string]any{"host": "http://127.0.0.1:50053/upstream/" + b.Name}
					continue
				}
				// Preserving caller assertions is necessary for federation, but an
				// unauthenticated upstream must never receive the caller bearer token.
				policies, ok := target["policies"].(map[string]any)
				if !ok {
					policies = map[string]any{}
					target["policies"] = policies
				}
				if policies["backendAuth"] == nil {
					modifier, ok := policies["requestHeaderModifier"].(map[string]any)
					if !ok {
						modifier = map[string]any{}
						policies["requestHeaderModifier"] = modifier
					}
					hasAuthorization := false
					if set, ok := modifier["set"].(map[string]any); ok {
						for key := range set {
							if strings.EqualFold(key, "authorization") {
								hasAuthorization = true
							}
						}
					}
					if !hasAuthorization {
						remove, _ := modifier["remove"].([]any)
						modifier["remove"] = append(remove, "authorization")
					}
				}
			}
			definition := map[string]any{"name": b.Name, "revision": b.Revision, "targets": b.MCP.Targets, "callerAuth": c.MCP.CallerAuth, "teamOAuth": b.TeamOAuth != nil}
			if b.TeamOAuth != nil {
				definition["allowedTools"] = b.TeamOAuth.AllowedTools
				if c.MCP.OAuthUpdates != nil {
					definition["oauthResource"] = oauthResource
					definition["oauthPublicKey"] = c.MCP.OAuthUpdates.PublicKey()
				}
			}
			definitions = append(definitions, definition)
		}
		config["mcpBackends"] = definitions
	}
	return config, nil
}

func validateIdentityMarkers(value any, path string) error {
	switch v := value.(type) {
	case map[string]any:
		if ref, ok := v["fromInstanceParameter"]; ok {
			name, ok := ref.(string)
			if !ok || !mcpconfig.ValidID(name) || len(v) != 1 || path != "/policies/backendAuth/oauthTokenExchange/clientAuth/clientId" {
				return errors.New("instance identity references supported only for federation clientId with a nonempty parameter name")
			}
			return nil
		}
		for k, item := range v {
			if err := validateIdentityMarkers(item, path+"/"+k); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := validateIdentityMarkers(item, path+"/[]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func operatorBroker(raw []byte, port int) (map[string]any, error) {
	var c struct {
		Installation string `json:"installation"`
		Auth         struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auth"`
		Gateway struct {
			ManagementURL string `json:"management_url"`
			TrustedCA     string `json:"management_trusted_ca"`
		} `json:"gateway"`
		Services  json.RawMessage `json:"services"`
		TeamOAuth json.RawMessage `json:"team_oauth,omitempty"`
	}
	if err := mcpconfig.DecodeOperator(raw, &c); err != nil {
		return nil, err
	}
	if c.Installation == "" || c.Auth.Username == "" || c.Auth.Password == "" || len(c.Services) == 0 {
		return nil, errors.New("broker installation, auth and services required")
	}
	// managementConfig adds CF's rotating client identity and mounted CA roots.
	if err := os.Setenv("MANAGEMENT_URL", c.Gateway.ManagementURL); err != nil {
		return nil, err
	}
	if err := os.Setenv("MANAGEMENT_TRUSTED_CA", c.Gateway.TrustedCA); err != nil {
		return nil, err
	}
	management, err := managementConfig()
	if err != nil {
		return nil, err
	}
	config := map[string]any{"listen": fmt.Sprintf("0.0.0.0:%d", port), "username": c.Auth.Username, "password": c.Auth.Password, "installation": c.Installation, "management_url": c.Gateway.ManagementURL, "services": c.Services}
	if len(c.TeamOAuth) != 0 {
		config["team_oauth"] = c.TeamOAuth
	}
	for k, v := range management {
		config[k] = v
	}
	return config, nil
}
