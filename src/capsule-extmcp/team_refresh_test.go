package main

import (
	mcpconfig "cf-mcp-config"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshClaimFencesConcurrentExchangeAndDisconnect(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var exchanges atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("refresh_token") != "original-refresh" {
			t.Error("invalid refresh request")
		}
		if exchanges.Add(1) != 1 {
			t.Error("same rotating token spent twice")
			w.WriteHeader(400)
			return
		}
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "replacement", "expires_in": 3600, "refresh_token": "rotated"})
	}))
	defer provider.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var cfg mcpconfig.GatewayOperator
	cfg.Capsules.LogID = "lab"
	cfg.MCP.OAuthUpdates = &mcpconfig.OAuthUpdates{StorageKey: strings.Repeat("12", 32)}
	d, err := newTeamDashboard(cfg)
	if err != nil {
		t.Fatal(err)
	}
	i := mcpconfig.Instance{ID: "instance", Installation: "lab", Generation: "gen", Backend: mcpconfig.Backend{Auth: mcpconfig.Auth{Method: mcpconfig.TeamOAuthMethod}}, Selections: []mcpconfig.BackendSelection{{Name: "test", Revision: "v1"}}}
	d.backends["test"] = oauthBackend{revision: "v1", endpoint: "https://tools.example/mcp", refreshEndpoint: provider.URL}
	id := mcpconfig.ManagedID("lab", i.ID, "")
	connection := oauthConnection{Name: id, Generation: i.Generation, Backend: "test", Revision: "v1", Operation: "connect", Sequence: 1}
	secret := upstreamSecret{Token: "old-access", Expires: time.Now(), Epoch: "epoch", Refresh: &mcpconfig.OAuthRefresh{Token: "original-refresh", Endpoint: provider.URL, ClientID: "client", Method: "none"}}
	connection.Sealed, err = d.seal(connectionPurpose(connection), secret)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var generation int64 = 1
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			if r.Header.Get("If-Match") != strconv.Quote(strconv.FormatInt(generation, 10)) {
				w.WriteHeader(409)
				return
			}
			var update struct {
				Resources []struct{ Value oauthConnection }
			}
			if json.NewDecoder(r.Body).Decode(&update) != nil || len(update.Resources) != 1 {
				w.WriteHeader(400)
				return
			}
			connection = update.Resources[0].Value
			generation++
			return
		}
		var value any = connection
		if strings.Contains(r.URL.Path, "/mcp.instance/") {
			value = map[string]any{"metadata": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"generation": generation, "resources": []any{map[string]any{"id": id, "value": value}}})
	}))
	defer admin.Close()
	d.admin, d.client = admin.URL, provider.Client()
	replica, err := newTeamDashboard(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replica.admin, replica.client = admin.URL, provider.Client()
	original := connection
	initial := int64(1)
	snapshot := oauthSnapshot{Generation: &initial}
	result := make(chan bool, 1)
	go func() { result <- d.refreshConnection(t.Context(), snapshot, original, secret) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never started")
	}
	if replica.refreshConnection(t.Context(), snapshot, original, secret) {
		t.Fatal("another replica claimed the same refresh")
	}
	mu.Lock()
	connection.Sealed, connection.RefreshAttempt, connection.Operation = "", "", "disconnect"
	connection.Sequence++
	generation++
	mu.Unlock()
	releaseOnce.Do(func() { close(release) })
	if <-result {
		t.Fatal("late refresh resurrected disconnected authorization")
	}
}
