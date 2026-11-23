package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	body, _ := json.Marshal(update)
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
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
		var current oauthConnection
		if s.find(id, &current) && current.Generation != i.Generation {
			current = oauthConnection{}
		}
		if current.Operation == update.Operation && current.Digest == digest {
			w.WriteHeader(204)
			return
		}
		if current.Operation != update.Previous || current.Operation == update.Operation || current.Sequence != update.Sequence {
			http.Error(w, "connection changed", 409)
			return
		}
		backend := i.Selections[0]
		c := oauthConnection{Name: id, Generation: i.Generation, Backend: backend.Name, Revision: backend.Revision, Operation: update.Operation, Digest: digest, Sequence: update.Sequence + 1}
		if update.Action == "connect" {
			access, err := update.Decrypt(d.updates.PrivateKey, i.ID, d.backends[backend.Name].endpoint)
			now := time.Now()
			if err != nil || !validToken(access.Token) || !access.Expires.After(now) || access.Expires.After(now.Add(24*time.Hour)) {
				http.Error(w, "invalid OAuth connection envelope", 400)
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
