package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

func (b *Broker) resolveShared(ctx context.Context, plan *OfferingPlan, params map[string]string) (*mcpconfig.Backend, []mcpconfig.BackendSelection, error) {
	var response struct {
		Resources []struct {
			Value struct {
				Name       string               `json:"name"`
				Revision   string               `json:"revision"`
				CallerAuth mcpconfig.CallerAuth `json:"callerAuth"`
				Targets    []map[string]any     `json:"targets"`
			} `json:"value"`
		} `json:"resources"`
	}
	if err := b.management(ctx, http.MethodGet, "mcp.backend", "", nil, &response); err != nil {
		return nil, nil, errors.New("cannot resolve shared MCP backend configuration")
	}
	var backend *mcpconfig.Backend
	selections := []mcpconfig.BackendSelection{}
	for _, name := range plan.Access.Backends {
		found := false
		for _, r := range response.Resources {
			v := r.Value
			if v.Name != name {
				continue
			}
			found = true
			if !resourceID.MatchString(v.Revision) || len(v.CallerAuth.Audiences) != 1 || v.CallerAuth.Issuer == "" || v.CallerAuth.JWKS.URL == "" {
				return nil, nil, errors.New("invalid shared MCP backend contract")
			}
			current := &mcpconfig.Backend{Name: name, Auth: mcpconfig.Auth{Method: "native", Issuer: v.CallerAuth.Issuer, JWKSURL: v.CallerAuth.JWKS.URL, Audience: v.CallerAuth.Audiences[0]}}
			if backend != nil { // Compare only caller trust, never shared upstream secrets.
				if backend.Auth.Issuer != current.Auth.Issuer || backend.Auth.Audience != current.Auth.Audience || backend.Auth.JWKSURL != current.Auth.JWKSURL {
					return nil, nil, errors.New("MCP backend caller trust mismatch")
				}
			}
			backend = current
			identities := map[string]mcpconfig.SelectedIdentity{}
			for _, target := range v.Targets {
				data, _ := json.Marshal(target)
				var native struct {
					Name     string `json:"name"`
					Policies struct {
						Auth struct {
							Exchange struct {
								Host   string `json:"host"`
								Client struct {
									ID json.RawMessage `json:"clientId"`
								} `json:"clientAuth"`
							} `json:"oauthTokenExchange"`
						} `json:"backendAuth"`
					} `json:"policies"`
				}
				_ = json.Unmarshal(data, &native)
				selected := mcpconfig.SelectedIdentity{BackendID: name, AuthMethod: "native"}
				var ref struct {
					Parameter string `json:"fromInstanceParameter"`
				}
				if json.Unmarshal(native.Policies.Auth.Exchange.Client.ID, &ref) == nil && ref.Parameter != "" {
					if params[ref.Parameter] == "" {
						return nil, nil, errors.New("missing required backend identity parameter")
					}
					selected.ClientID = params[ref.Parameter]
				}
				if native.Policies.Auth.Exchange.Host != "" {
					selected.AuthMethod = "federated_client_credentials"
					selected.Authority = native.Policies.Auth.Exchange.Host
				}
				identities[native.Name] = selected
			}
			selections = append(selections, mcpconfig.BackendSelection{Name: name, Revision: v.Revision, Parameters: params, Identities: identities})
		}
		if !found {
			return nil, nil, errors.New("unknown shared MCP backend")
		}
	}
	return backend, selections, nil
}
