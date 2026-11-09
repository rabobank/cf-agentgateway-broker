package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type mcpState struct {
	pool         *pgxpool.Pool
	installation string
}

func generation() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func openMCPState(ctx context.Context, uri, installation string) (*mcpState, error) {
	p, e := pgxpool.New(ctx, uri)
	if e != nil {
		return nil, e
	}
	s := &mcpState{p, installation}
	_, e = p.Exec(ctx, `CREATE TABLE IF NOT EXISTS agw_mcp_instances (installation text NOT NULL,id text NOT NULL,value jsonb NOT NULL,PRIMARY KEY(installation,id)); CREATE TABLE IF NOT EXISTS agw_mcp_bindings (installation text NOT NULL,instance_id text NOT NULL,id text NOT NULL,value jsonb NOT NULL,PRIMARY KEY(installation,instance_id,id),FOREIGN KEY(installation,instance_id) REFERENCES agw_mcp_instances(installation,id) ON DELETE CASCADE)`)
	if e != nil {
		p.Close()
		return nil, e
	}
	return s, nil
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

// Every operation for an instance holds the same session-scoped lock. The
// grant is authoritative; route writes are idempotent and revocation commits
// before route cleanup, so delayed management writes cannot restore access.
func (b *Broker) serveMCP(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete && r.Method != http.MethodPatch {
		return false
	}
	var req Request
	if r.Method == http.MethodPut || r.Method == http.MethodPatch {
		data, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if e != nil {
			fail(w, 400, "invalid request")
			return true
		}
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		if json.Unmarshal(data, &req) != nil {
			return false
		}
	} else {
		req.PlanID = r.URL.Query().Get("plan_id")
		req.ServiceID = r.URL.Query().Get("service_id")
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
	if b.state == nil {
		fail(w, 503, "MCP state unavailable")
		return true
	}
	ctx := r.Context()
	conn, e := b.state.pool.Acquire(ctx)
	if e != nil {
		fail(w, 503, "MCP state unavailable")
		return true
	}
	defer conn.Release()
	digest := sha256.Sum256([]byte(b.cfg.Installation + "/" + parts[2]))
	lock := int64(binary.BigEndian.Uint64(digest[:8]))
	if _, e = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); e != nil {
		fail(w, 503, "cannot lock instance")
		return true
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, "SELECT pg_advisory_unlock($1)", lock); err != nil {
			_ = conn.Hijack().Close(cleanup)
		}
	}()
	tx, e := conn.Begin(ctx)
	if e != nil {
		fail(w, 503, "cannot begin MCP transaction")
		return true
	}
	defer tx.Rollback(ctx)
	var raw []byte
	e = tx.QueryRow(ctx, "SELECT value FROM agw_mcp_instances WHERE installation=$1 AND id=$2", b.cfg.Installation, parts[2]).Scan(&raw)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		fail(w, 503, "cannot resolve instance")
		return true
	}
	var instance mcpconfig.Instance
	if len(raw) > 0 && json.Unmarshal(raw, &instance) != nil {
		fail(w, 503, "invalid stored instance")
		return true
	}
	if len(raw) > 0 && (instance.PlanID != req.PlanID || instance.ServiceID != req.ServiceID) {
		fail(w, 409, "instance service or plan mismatch")
		return true
	}
	if len(parts) == 3 {
		if r.Method == http.MethodDelete {
			if len(raw) == 0 {
				write(w, 410, map[string]any{})
				return true
			}
			rows, err := tx.Query(ctx, "SELECT value FROM agw_mcp_bindings WHERE installation=$1 AND instance_id=$2", b.cfg.Installation, parts[2])
			if err != nil {
				fail(w, 503, "cannot resolve bindings")
				return true
			}
			var bindings []mcpconfig.Binding
			for rows.Next() {
				var data []byte
				if rows.Scan(&data) != nil {
					rows.Close()
					fail(w, 503, "cannot read bindings")
					return true
				}
				var v mcpconfig.Binding
				if json.Unmarshal(data, &v) != nil {
					rows.Close()
					fail(w, 503, "invalid binding")
					return true
				}
				bindings = append(bindings, v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				fail(w, 503, "cannot read bindings")
				return true
			}
			_, e = tx.Exec(ctx, `UPDATE agw_mcp_instances SET value=jsonb_set(value,'{revoked}','true') WHERE installation=$1 AND id=$2`, b.cfg.Installation, parts[2])
			if e != nil || tx.Commit(ctx) != nil {
				fail(w, 503, "cannot revoke instance")
				return true
			}
			for _, v := range bindings {
				if b.deleteRoute(ctx, v) != nil {
					fail(w, 503, "instance revoked; route cleanup failed, retry")
					return true
				}
			}
			if _, e = conn.Exec(ctx, "DELETE FROM agw_mcp_instances WHERE installation=$1 AND id=$2", b.cfg.Installation, parts[2]); e != nil {
				fail(w, 503, "cannot finish instance cleanup")
				return true
			}
			write(w, 200, map[string]any{})
			return true
		}
		params, err := backend.Resolve(req.Parameters)
		if err != nil {
			fail(w, 400, err.Error())
			return true
		}
		fp := fingerprint(req)
		if len(raw) > 0 {
			if instance.Revoked {
				fail(w, 409, "instance is being deleted")
				return true
			}
			if instance.Fingerprint != fp {
				fail(w, 409, "instance parameters conflict")
			} else {
				write(w, 200, map[string]any{})
			}
			return true
		}
		// Protect the shared OSB namespace against an inference instance collision.
		legacy, err := b.find(ctx, parts[2])
		if err != nil {
			fail(w, 503, "cannot check instance ownership")
			return true
		}
		if legacy != nil {
			fail(w, 409, "instance already exists")
			return true
		}
		instance = mcpconfig.Instance{ID: parts[2], Installation: b.cfg.Installation, ServiceID: req.ServiceID, PlanID: req.PlanID, Org: req.OrganizationGUID, Space: req.SpaceGUID, Fingerprint: fp, Parameters: params, Backend: *backend}
		instance.Generation, e = generation()
		if e != nil {
			fail(w, 500, "cannot allocate instance generation")
			return true
		}
		data, _ := json.Marshal(instance)
		_, e = tx.Exec(ctx, "INSERT INTO agw_mcp_instances VALUES($1,$2,$3)", b.cfg.Installation, instance.ID, data)
		if e != nil || tx.Commit(ctx) != nil {
			fail(w, 503, "cannot provision MCP instance")
			return true
		}
		write(w, 201, map[string]any{})
		return true
	}
	if len(raw) == 0 {
		if r.Method == http.MethodDelete {
			write(w, 410, map[string]any{})
		} else {
			fail(w, 404, "instance not found")
		}
		return true
	}
	if instance.Revoked {
		fail(w, 409, "instance is being deleted")
		return true
	}
	binding := mcpconfig.Binding{ID: parts[4], Instance: instance}
	if r.Method == http.MethodDelete {
		err := tx.QueryRow(ctx, "SELECT value FROM agw_mcp_bindings WHERE installation=$1 AND instance_id=$2 AND id=$3", b.cfg.Installation, instance.ID, binding.ID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			write(w, 410, map[string]any{})
			return true
		}
		if err != nil || json.Unmarshal(raw, &binding) != nil {
			fail(w, 503, "cannot read binding for cleanup")
			return true
		}
		_, e = tx.Exec(ctx, `UPDATE agw_mcp_bindings SET value=jsonb_set(value,'{revoked}','true') WHERE installation=$1 AND instance_id=$2 AND id=$3`, b.cfg.Installation, instance.ID, binding.ID)
		if e != nil || tx.Commit(ctx) != nil {
			fail(w, 503, "cannot revoke binding")
			return true
		}
		if b.deleteRoute(ctx, binding) != nil {
			fail(w, 503, "binding revoked; route cleanup failed, retry")
			return true
		}
		if _, e = conn.Exec(ctx, "DELETE FROM agw_mcp_bindings WHERE installation=$1 AND instance_id=$2 AND id=$3", b.cfg.Installation, instance.ID, binding.ID); e != nil {
			fail(w, 503, "cannot finish binding cleanup")
			return true
		}
		write(w, 200, map[string]any{})
		return true
	}
	if len(req.Parameters) > 0 {
		fail(w, 400, "binding parameters are not supported; configure identity on the instance")
		return true
	}
	app, ok := req.BindResource["app_guid"].(string)
	if !ok || app == "" || req.Context["platform"] != "cloudfoundry" {
		fail(w, 422, "MCP plans require an app binding with a service account")
		return true
	}
	accountJSON, _ := json.Marshal(req.Context["service_account"])
	if json.Unmarshal(accountJSON, &binding.Account) != nil || binding.Account.Issuer != instance.Backend.Auth.Issuer || binding.Account.Subject == "" {
		fail(w, 422, "trusted service account required")
		return true
	}
	e = tx.QueryRow(ctx, "SELECT value FROM agw_mcp_bindings WHERE installation=$1 AND instance_id=$2 AND id=$3", b.cfg.Installation, instance.ID, binding.ID).Scan(&raw)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		fail(w, 503, "cannot resolve binding")
		return true
	}
	if e == nil {
		var existing mcpconfig.Binding
		if json.Unmarshal(raw, &existing) != nil || existing.Revoked || existing.Account != binding.Account {
			fail(w, 409, "binding identity conflict")
			return true
		}
		binding = existing
	} else {
		binding.Generation, e = generation()
		if e != nil {
			fail(w, 500, "cannot allocate binding generation")
			return true
		}
	}
	if b.putRoute(ctx, binding) != nil {
		fail(w, 503, "cannot configure binding route")
		return true
	}
	data, _ := json.Marshal(binding)
	_, e = tx.Exec(ctx, "INSERT INTO agw_mcp_bindings VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", b.cfg.Installation, instance.ID, binding.ID, data)
	if e != nil || tx.Commit(ctx) != nil {
		fail(w, 503, "cannot commit binding grant")
		return true
	}
	base := strings.TrimSuffix(strings.TrimRight(b.cfg.MCPURL, "/"), "/mcp")
	write(w, 201, map[string]any{"credentials": map[string]any{"mcp_url": base + binding.Path(), "audience": instance.Backend.Auth.Audience, "issuer": binding.Account.Issuer, "subject": binding.Account.Subject, "auth_method": instance.Backend.Auth.Method, "trusted_ca": b.trustedCA}})
	return true
}
func (b *Broker) putRoute(ctx context.Context, binding mcpconfig.Binding) error {
	return b.routeAPI(ctx, http.MethodPut, "", map[string]any{"resources": []any{map[string]any{"value": binding.Route()}}})
}
func (b *Broker) deleteRoute(ctx context.Context, binding mcpconfig.Binding) error {
	err := b.routeAPI(ctx, http.MethodDelete, "/"+url.PathEscape(binding.RouteName()), nil)
	var remote *apiError
	if errors.As(err, &remote) && remote.status == 404 {
		return nil
	}
	return err
}
func (b *Broker) routeAPI(ctx context.Context, method, path string, body any) error {
	// Reuse transport and operator management authentication, selecting the
	// documented traffic.route resource collection rather than gateway tables.
	clone := *b
	clone.cfg.ManagementURL = strings.TrimRight(b.cfg.ManagementURL, "/")
	return clone.management(ctx, method, "traffic.route", path, body, nil)
}
