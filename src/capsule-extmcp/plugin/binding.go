package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type BindingRepository interface {
	AuthorizeBinding(context.Context, string, string, string, string) (Identity, error)
}

// BindingAuthHandler is loopback-only. Issuer and subject are populated by the
// gateway's JWT policy, never taken from caller-supplied identity headers.
func BindingAuthHandler(repo BindingRepository) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Binding  string `json:"binding_id"`
			Instance string `json:"instance_id"`
			Issuer   string `json:"issuer"`
			Subject  string `json:"subject"`
			Target   string `json:"target"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || in.Binding == "" || in.Instance == "" || in.Issuer == "" || in.Subject == "" || in.Target == "" {
			http.Error(w, "binding context required", 403)
			return
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			http.Error(w, "invalid context", 403)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		identity, err := repo.AuthorizeBinding(ctx, in.Instance, in.Binding, in.Issuer, in.Subject)
		if err != nil || identity.Target != in.Target {
			http.Error(w, "binding denied", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(identity)
	})
}
