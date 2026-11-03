package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// AuthHandler is the loopback HTTP extAuthz endpoint. Unlike MCP method hooks,
// it also gates initialization, session streams, and session deletion.
func AuthHandler(repo Repository) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		values := r.Header.Values("Authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			http.Error(w, "credential required", http.StatusForbidden)
			return
		}
		identity, err := repo.Authenticate(ctx, strings.TrimPrefix(values[0], "Bearer "))
		if err != nil {
			http.Error(w, "credential rejected", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(identity)
	})
}
