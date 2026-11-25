package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (d *teamDashboard) canRefresh(c oauthConnection, secret upstreamSecret) bool {
	r := secret.Refresh
	return c.RefreshAttempt == "" && r != nil && r.Valid() && r.Endpoint == d.backends[c.Backend].refreshEndpoint
}

func (d *teamDashboard) refreshConnection(ctx context.Context, s oauthSnapshot, c oauthConnection, secret upstreamSecret) bool {
	// Claim the exchange durably before spending a potentially single-use
	// token. An uncertain outcome must never cause another replica to retry it.
	attempt, err := nonce()
	if err != nil {
		return false
	}
	c.RefreshAttempt = attempt
	if !d.writeConnection(ctx, s, c) {
		return false
	}
	updated, ok := d.exchangeRefresh(ctx, secret, d.backends[c.Backend].endpoint)
	if !ok {
		return false
	}
	sealed, err := d.seal(connectionPurpose(c), updated)
	if err != nil {
		return false
	}
	// Finish even if the tool caller disconnected. Never write over a newer
	// connection, a disconnect, or a deprovisioned instance.
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	defer cancel()
	for range 3 {
		snapshot, err := d.snapshot(commit, oauthKind, c.Name)
		var current oauthConnection
		if err != nil || !snapshot.find(c.Name, &current) || current.RefreshAttempt != attempt || current.Generation != c.Generation {
			return false
		}
		instance, gen, ok := d.instanceAt(commit, c.Name)
		if !ok || instance.Generation != c.Generation {
			return false
		}
		if gen == nil || snapshot.Generation == nil || *gen != *snapshot.Generation {
			continue
		}
		current.Sealed, current.RefreshAttempt = sealed, ""
		if d.writeConnection(commit, snapshot, current) {
			return true
		}
	}
	return false
}

func (d *teamDashboard) exchangeRefresh(ctx context.Context, secret upstreamSecret, resource string) (upstreamSecret, bool) {
	r := *secret.Refresh
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r.Token}, "resource": {resource}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint, nil)
	if err != nil {
		return upstreamSecret{}, false
	}
	mcpconfig.ApplyOAuthClientAuth(req, form, r.Method, r.ClientID, r.ClientSecret)
	encoded := form.Encode()
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader(encoded)), int64(len(encoded))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.client.Do(req)
	if err != nil {
		return upstreamSecret{}, false
	}
	defer resp.Body.Close()
	var result struct {
		Access  string  `json:"access_token"`
		Type    *string `json:"token_type"`
		Expires *int64  `json:"expires_in"`
		Refresh string  `json:"refresh_token"`
	}
	if resp.StatusCode != http.StatusOK || readJSON(resp.Body, 128<<10, &result) != nil || !validToken(result.Access) || (result.Type != nil && !strings.EqualFold(*result.Type, "bearer")) {
		return upstreamSecret{}, false
	}
	lifetime, err := mcpconfig.OAuthAccessLifetime(result.Expires)
	if err != nil {
		return upstreamSecret{}, false
	}
	if result.Refresh != "" {
		r.Token = result.Refresh
	}
	if !r.Valid() {
		return upstreamSecret{}, false
	}
	secret.Token, secret.Expires, secret.Refresh = result.Access, time.Now().UTC().Add(lifetime), &r
	return secret, true
}
