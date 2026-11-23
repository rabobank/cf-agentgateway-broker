package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (d *teamDashboard) connectionStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id := r.PathValue("instance")
	i, ok := d.instance(ctx, id)
	if !ok {
		http.Error(w, "connection unavailable", 404)
		return
	}
	s, err := d.snapshot(ctx, oauthKind, id)
	if err != nil {
		http.Error(w, "connection unavailable", 503)
		return
	}
	var c oauthConnection
	status := mcpconfig.OAuthStatus{Generation: i.Generation}
	if s.find(id, &c) && c.Generation == i.Generation {
		status.Operation = c.Operation
		status.Sequence = c.Sequence
	}
	if access, ok := d.usable(s, id, i); ok {
		status.Connected = true
		status.Expires = &access.Expires
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(status)
}

func (d *teamDashboard) updateConnection(w http.ResponseWriter, r *http.Request) {
	var update mcpconfig.OAuthUpdate
	if readJSON(http.MaxBytesReader(w, r.Body, 24<<10), 24<<10, &update) != nil || update.Validate() != nil {
		http.Error(w, "invalid OAuth update", 400)
		return
	}
	id := r.PathValue("instance")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Each retry uses a fresh snapshot; the operation predecessor fences stale
	// requests, while the generation precondition fences concurrent lifecycle changes.
	for range 3 {
		s, err := d.snapshot(ctx, oauthKind, id)
		if err != nil {
			break
		}
		i, ok := d.instance(ctx, id)
		if !ok || i.Generation != update.Generation {
			http.Error(w, "instance changed", 409)
			return
		}
		backend := i.Selections[0]
		if update.Resource != d.backends[backend.Name].endpoint {
			http.Error(w, "upstream resource changed", 409)
			return
		}
		var current oauthConnection
		if s.find(id, &current) && current.Generation != i.Generation {
			current = oauthConnection{}
		}
		if current.Operation == update.Operation {
			if d.sameConnectionUpdate(current, update, backend) {
				w.WriteHeader(204)
			} else {
				http.Error(w, "connection changed", 409)
			}
			return
		}
		if current.Operation != update.Previous || current.Sequence != update.Sequence {
			http.Error(w, "connection changed", 409)
			return
		}
		c := oauthConnection{Name: id, Generation: i.Generation, Backend: backend.Name, Revision: backend.Revision, Operation: update.Operation, Previous: update.Previous, Sequence: update.Sequence + 1}
		if update.Action == "connect" {
			access := update.Access
			now := time.Now()
			if !validToken(access.Token) || !access.Expires.After(now) || access.Expires.After(now.Add(24*time.Hour)) {
				http.Error(w, "invalid OAuth access token or expiry", 400)
				return
			}
			c.Sealed, err = d.seal(connectionPurpose(c), upstreamSecret{Token: access.Token, Expires: access.Expires, Epoch: update.Operation})
			if err != nil {
				break
			}
		}
		if d.writeConnection(ctx, s, c) {
			w.WriteHeader(204)
			return
		}
	}
	http.Error(w, "connection update unavailable; retry the same operation", 503)
}

func (d *teamDashboard) sameConnectionUpdate(c oauthConnection, update mcpconfig.OAuthUpdate, backend mcpconfig.BackendSelection) bool {
	if c.Backend != backend.Name || c.Revision != backend.Revision || c.Previous != update.Previous || c.Sequence != update.Sequence+1 {
		return false
	}
	if update.Action == "disconnect" {
		return c.Sealed == ""
	}
	var stored upstreamSecret
	return d.open(connectionPurpose(c), c.Sealed, &stored) && stored.Epoch == update.Operation &&
		stored.Token == update.Access.Token && stored.Expires.Equal(update.Access.Expires)
}
