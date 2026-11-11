package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func generation() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
func managedID(installation, instance, binding string) string {
	data, _ := json.Marshal([]string{installation, instance, binding})
	hash := sha256.Sum256(data)
	return "cf-mcp-" + hex.EncodeToString(hash[:])
}

type managedResource struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}
type managedValue struct {
	Name     string          `json:"name"`
	Instance string          `json:"instance,omitempty"`
	Metadata json.RawMessage `json:"metadata"`
	Route    map[string]any  `json:"route,omitempty"`
}

func (b *Broker) backend(plan string) *mcpconfig.Backend {
	for i := range b.cfg.MCPServers {
		v := &b.cfg.MCPServers[i]
		if v.PlanID != "" && v.PlanID == plan {
			return v
		}
	}
	return nil
}
func (b *Broker) managed(ctx context.Context, kind, id string) (json.RawMessage, error) {
	var list struct {
		Resources []managedResource `json:"resources"`
	}
	if err := b.management(ctx, http.MethodGet, kind, "", nil, &list); err != nil {
		return nil, err
	}
	for _, v := range list.Resources {
		if v.ID == id {
			return v.Value, nil
		}
	}
	return nil, nil
}
func (b *Broker) saveManaged(ctx context.Context, kind string, value managedValue) error {
	return b.management(ctx, http.MethodPut, kind, "", map[string]any{"resources": []any{map[string]any{"value": value}}}, nil)
}
func (b *Broker) deleteManaged(ctx context.Context, kind, id string) error {
	err := b.management(ctx, http.MethodDelete, kind, "/"+url.PathEscape(id), nil, nil)
	var remote *apiError
	if errors.As(err, &remote) && remote.status == 404 {
		return nil
	}
	return err
}
func (b *Broker) instance(ctx context.Context, id string) (*mcpconfig.Instance, error) {
	raw, err := b.managed(ctx, "mcp.instance", managedID(b.cfg.Installation, id, ""))
	if err != nil || raw == nil {
		return nil, err
	}
	var resource managedValue
	var instance mcpconfig.Instance
	if json.Unmarshal(raw, &resource) != nil || json.Unmarshal(resource.Metadata, &instance) != nil || instance.Installation != b.cfg.Installation || instance.ID != id {
		return nil, errors.New("invalid managed instance")
	}
	return &instance, nil
}

func (b *Broker) serveMCP(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete && r.Method != http.MethodPatch {
		return false
	}
	var req Request
	if r.Method == http.MethodPut || r.Method == http.MethodPatch {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			fail(w, 400, "invalid request")
			return true
		}
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		if json.Unmarshal(data, &req) != nil {
			return false
		}
	} else {
		req.ServiceID = r.URL.Query().Get("service_id")
		req.PlanID = r.URL.Query().Get("plan_id")
	}
	backend := b.backend(req.PlanID)
	if backend == nil {
		return false
	}
	if req.ServiceID != b.cfg.ServiceID {
		fail(w, 400, "unknown service")
		return true
	}
	if r.Method == http.MethodPatch {
		fail(w, 422, "plan updates are not supported")
		return true
	}
	if len(parts) != 3 && !(len(parts) == 5 && parts[3] == "service_bindings" && resourceID.MatchString(parts[4])) {
		fail(w, 404, "not found")
		return true
	}
	ctx := r.Context()
	id := parts[2]
	instanceID := managedID(b.cfg.Installation, id, "")
	instance, err := b.instance(ctx, id)
	if err != nil {
		fail(w, 503, "cannot recover managed instance")
		return true
	}
	if instance != nil && (instance.PlanID != req.PlanID || instance.ServiceID != req.ServiceID) {
		fail(w, 409, "instance service or plan mismatch")
		return true
	}
	if len(parts) == 3 {
		if r.Method == http.MethodDelete {
			// Delete the parent first: live authorization rejects all remaining grants.
			if b.deleteManaged(ctx, "mcp.instance", instanceID) != nil {
				fail(w, 503, "cannot revoke instance")
				return true
			}
			var grants struct {
				Resources []managedResource `json:"resources"`
			}
			if b.management(ctx, http.MethodGet, "mcp.grant", "", nil, &grants) != nil {
				fail(w, 503, "cannot list instance grants")
				return true
			}
			for _, v := range grants.Resources {
				var grant managedValue
				if json.Unmarshal(v.Value, &grant) != nil {
					fail(w, 503, "invalid grant")
					return true
				}
				if grant.Instance == instanceID && b.deleteManaged(ctx, "mcp.grant", v.ID) != nil {
					fail(w, 503, "cannot clean up grant")
					return true
				}
			}
			if instance == nil {
				write(w, 410, map[string]any{})
			} else {
				write(w, 200, map[string]any{})
			}
			return true
		}
		fp := fingerprint(req)
		if instance != nil {
			if instance.Fingerprint != fp {
				fail(w, 409, "instance parameters conflict")
			} else {
				write(w, 200, map[string]any{})
			}
			return true
		}
		params, err := backend.Resolve(req.Parameters)
		if err != nil {
			fail(w, 400, err.Error())
			return true
		}
		legacy, err := b.find(ctx, id)
		if err != nil {
			fail(w, 503, "cannot check instance ownership")
			return true
		}
		if legacy != nil {
			fail(w, 409, "instance already exists")
			return true
		}
		gen, err := generation()
		if err != nil {
			fail(w, 500, "cannot allocate instance generation")
			return true
		}
		instance = &mcpconfig.Instance{ID: id, Installation: b.cfg.Installation, ServiceID: req.ServiceID, PlanID: req.PlanID, Org: req.OrganizationGUID, Space: req.SpaceGUID, Fingerprint: fp, Parameters: params, Backend: *backend, Generation: gen}
		data, _ := json.Marshal(instance)
		if err = b.saveManaged(ctx, "mcp.instance", managedValue{Name: instanceID, Metadata: data}); err != nil {
			fail(w, 503, "cannot publish instance configuration")
			return true
		}
		write(w, 201, map[string]any{})
		return true
	}
	grantID := managedID(b.cfg.Installation, id, parts[4])
	if r.Method == http.MethodDelete {
		if b.deleteManaged(ctx, "mcp.grant", grantID) != nil {
			fail(w, 503, "cannot revoke binding")
			return true
		}
		write(w, 200, map[string]any{})
		return true
	}
	if instance == nil {
		fail(w, 404, "instance not found")
		return true
	}
	if len(req.Parameters) > 0 {
		fail(w, 400, "configure identity on the instance")
		return true
	}
	app, ok := req.BindResource["app_guid"].(string)
	if !ok || app == "" || req.Context["platform"] != "cloudfoundry" {
		fail(w, 422, "app service account required")
		return true
	}
	binding := mcpconfig.Binding{ID: parts[4], Instance: *instance}
	account, _ := json.Marshal(req.Context["service_account"])
	if json.Unmarshal(account, &binding.Account) != nil || binding.Account.Issuer != instance.Backend.Auth.Issuer || binding.Account.Subject == "" {
		fail(w, 422, "trusted service account required")
		return true
	}
	raw, err := b.managed(ctx, "mcp.grant", grantID)
	if err != nil {
		fail(w, 503, "cannot recover binding")
		return true
	}
	if raw != nil {
		var resource managedValue
		var existing mcpconfig.Binding
		if json.Unmarshal(raw, &resource) != nil || json.Unmarshal(resource.Metadata, &existing) != nil || existing.Account != binding.Account || existing.Instance.Generation != instance.Generation {
			fail(w, 409, "binding identity conflict")
			return true
		}
		binding = existing
	} else {
		binding.Generation, err = generation()
		if err != nil {
			fail(w, 500, "cannot allocate binding generation")
			return true
		}
		data, _ := json.Marshal(binding)
		if err = b.saveManaged(ctx, "mcp.grant", managedValue{Name: grantID, Instance: instanceID, Metadata: data, Route: binding.Route()}); err != nil {
			fail(w, 503, "cannot publish binding configuration")
			return true
		}
	}
	base := strings.TrimSuffix(strings.TrimRight(b.cfg.MCPURL, "/"), "/mcp")
	write(w, 201, map[string]any{"credentials": map[string]any{"mcp_url": base + binding.Path(), "audience": instance.Backend.Auth.Audience, "issuer": binding.Account.Issuer, "subject": binding.Account.Subject, "auth_method": instance.Backend.Auth.Method, "trusted_ca": b.trustedCA}})
	return true
}
