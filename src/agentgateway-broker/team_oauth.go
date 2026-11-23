package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type teamOAuthBroker struct {
	BindingKey string `json:"binding_key"`
}

func (c teamOAuthBroker) key() []byte {
	key, err := hex.DecodeString(c.BindingKey)
	if err != nil {
		return nil
	}
	return key
}

func (c teamOAuthBroker) bearer(binding mcpconfig.Binding) (string, string) {
	mac := hmac.New(sha256.New, c.key())
	mac.Write([]byte("cf-agentgateway/mcp-binding/v1/" + binding.TargetName()))
	token := hex.EncodeToString(mac.Sum(nil))
	hash := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(hash[:])
}

func (b *Broker) oauthManagement(ctx context.Context, method, instance string, body any, out any) error {
	return b.managementURL(ctx, method, "mcp.oauth", "/"+instance, strings.TrimRight(b.cfg.ManagementURL, "/")+"/internal/mcp-oauth/"+url.PathEscape(instance), body, out)
}

func (b *Broker) updateOAuth(w http.ResponseWriter, r *http.Request, instance *mcpconfig.Instance, req Request) {
	_, plan := b.cfg.offering(instance.ServiceID, instance.PlanID)
	if b.cfg.TeamOAuth == nil || plan == nil || plan.OAuthProfile == nil || instance.Backend.Auth.Method != mcpconfig.TeamOAuthMethod {
		fail(w, 422, "instance updates are not supported")
		return
	}
	var update mcpconfig.OAuthUpdate
	data, _ := json.Marshal(req.Parameters[mcpconfig.OAuthProfileField])
	if len(req.Parameters) != 1 || strictJSON(data, &update) != nil || update.Validate() != nil {
		fail(w, 400, "invalid OAuth connection parameters")
		return
	}
	if update.Generation != instance.Generation {
		fail(w, 409, "service instance changed; rediscover the connection")
		return
	}
	if update.Resource != plan.OAuthProfile.Resource {
		fail(w, 409, "OAuth service plan changed; rediscover the connection")
		return
	}
	err := b.oauthManagement(r.Context(), http.MethodPatch, managedID(b.cfg.Installation, instance.ID, ""), update, nil)
	if err != nil {
		var api *apiError
		if errors.As(err, &api) && (api.status == 409 || api.status == 400) {
			fail(w, api.status, "OAuth connection update rejected; check the current status")
			return
		}
		fail(w, 503, "OAuth connection update unavailable")
		return
	}
	write(w, 200, map[string]any{})
}
