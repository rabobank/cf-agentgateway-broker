// Package postgres supplies durable storage for a shared producer log. The
// upstream memory backend validates CLL transitions; PostgreSQL supplies atomic
// commits and cross-process serialization, without copying private upstream code.
package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"cf-agentgateway-capsule/plugin"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/store/memory"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool    *pgxpool.Pool
	logID   string
	trusted []ed25519.PublicKey
	closed  atomic.Bool
}

func (s *Store) LogID() string { return s.logID }

func Open(ctx context.Context, pool *pgxpool.Pool, logID string, trusted []ed25519.PublicKey) (*Store, error) {
	if err := cll.ValidateIdentifier(logID); err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	return &Store{pool: pool, logID: logID, trusted: trusted}, nil
}

func (s *Store) Init(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize schema provisioning between CF sidecars during rolling starts.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(684119329)"); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS agw_capsule_logs (
 log_id text PRIMARY KEY, entry_count bigint NOT NULL DEFAULT 0,
 state jsonb NOT NULL DEFAULT '{"Size":0,"IndexedSeq":0}');
CREATE TABLE IF NOT EXISTS agw_capsule_entries (
 log_id text NOT NULL REFERENCES agw_capsule_logs(log_id), seq bigint NOT NULL,
 identity bytea NOT NULL CHECK(octet_length(identity)=32), appended_ms bigint NOT NULL,
 PRIMARY KEY(log_id,seq), UNIQUE(log_id,identity));
CREATE TABLE IF NOT EXISTS agw_capsule_nodes (
 log_id text NOT NULL REFERENCES agw_capsule_logs(log_id), position bigint NOT NULL,
 hash bytea NOT NULL CHECK(octet_length(hash)=32), PRIMARY KEY(log_id,position));
CREATE TABLE IF NOT EXISTS agw_capsule_witnesses (
 log_id text NOT NULL REFERENCES agw_capsule_logs(log_id), witness_id text NOT NULL,
 checkpoint_size bigint NOT NULL, state jsonb NOT NULL,
 PRIMARY KEY(log_id,witness_id,checkpoint_size));
CREATE TABLE IF NOT EXISTS agw_capsules (
 log_id text NOT NULL REFERENCES agw_capsule_logs(log_id), capsule_id text NOT NULL,
 installation text NOT NULL, instance_id text NOT NULL, observed_at timestamptz NOT NULL,
 parent_id text, record jsonb NOT NULL, checksum text NOT NULL,
 PRIMARY KEY(log_id,capsule_id));
CREATE UNIQUE INDEX IF NOT EXISTS agw_capsules_one_outcome ON agw_capsules(log_id,parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS agw_capsules_instance_time ON agw_capsules(installation,instance_id,observed_at,capsule_id);`)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO agw_capsule_logs(log_id) VALUES($1) ON CONFLICT DO NOTHING", s.logID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return cll.ErrClosed
	}
	return nil
}
func (s *Store) Close() error { s.closed.Store(true); return nil }

func (s *Store) Save(ctx context.Context, c plugin.Capsule) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if _, err := artifact.Verify(artifact.Record{CapsuleID: c.ID, Capsule: c.Bytes, ProducerEnvelope: c.Envelope}, s.trusted); err != nil {
		return err
	}
	// The indexed identity is derived from signed content, never caller labels.
	var payload struct {
		Gateway struct {
			plugin.Identity
			Phase    string   `json:"phase"`
			Tool     string   `json:"tool"`
			Backends []string `json:"backends"`
		} `json:"gateway"`
		Chain struct {
			Parent string `json:"parent_capsule_id"`
		} `json:"chain"`
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(c.Bytes, &payload); err != nil {
		return err
	}
	if payload.Gateway.Identity != c.Identity || payload.Gateway.Phase != c.Phase || payload.Gateway.Tool != c.Tool || !slices.Equal(payload.Gateway.Backends, c.Backends) || c.Identity.Instance == "" || c.Identity.Installation == "" {
		return artifact.ErrInvalid
	}
	if payload.Chain.Parent != c.Parent || !payload.Timestamp.Equal(c.ObservedAt.Truncate(time.Microsecond)) {
		return artifact.ErrInvalid
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	checksum := hex.EncodeToString(sum[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var existing string
	err = tx.QueryRow(ctx, "SELECT checksum FROM agw_capsules WHERE log_id=$1 AND capsule_id=$2", s.logID, c.ID).Scan(&existing)
	if err == nil {
		if existing != checksum {
			return artifact.ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// Allocate the CLL sequence first so retries serialize on the same log row.
	id, err := hex.DecodeString(c.ID)
	if err != nil {
		return err
	}
	if _, err = s.appendTx(ctx, tx, cll.AppendInput{Value: id, AppendedAt: c.ObservedAt}); err != nil {
		return err
	}
	var parent any
	if c.Parent != "" {
		parent = c.Parent
	}
	_, err = tx.Exec(ctx, `INSERT INTO agw_capsules(log_id,capsule_id,installation,instance_id,observed_at,parent_id,record,checksum)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(log_id,capsule_id) DO NOTHING`, s.logID, c.ID, c.Identity.Installation, c.Identity.Instance, c.ObservedAt, parent, raw, checksum)
	if err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, "SELECT checksum FROM agw_capsules WHERE log_id=$1 AND capsule_id=$2", s.logID, c.ID).Scan(&existing); err != nil {
		return err
	}
	if existing != checksum {
		return artifact.ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *Store) Get(ctx context.Context, id string) (plugin.Capsule, error) {
	if err := s.check(ctx); err != nil {
		return plugin.Capsule{}, err
	}
	var raw []byte
	var checksum string
	if err := s.pool.QueryRow(ctx, "SELECT record,checksum FROM agw_capsules WHERE log_id=$1 AND capsule_id=$2", s.logID, id).Scan(&raw, &checksum); err != nil {
		return plugin.Capsule{}, err
	}
	var c plugin.Capsule
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	canonical, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != checksum || c.ID != id {
		return plugin.Capsule{}, artifact.ErrCorrupt
	}
	_, err = artifact.Verify(artifact.Record{CapsuleID: c.ID, Capsule: c.Bytes, ProducerEnvelope: c.Envelope}, s.trusted)
	return c, err
}

func (s *Store) Authenticate(ctx context.Context, token string) (plugin.Identity, error) {
	var value []byte
	// Read the authoritative broker-managed resource: revocations take effect on
	// the next MCP request and no second credential lifecycle is introduced.
	err := s.pool.QueryRow(ctx, `SELECT value_json FROM agw_config_resources WHERE kind='llm.apiKey' AND deleted_at IS NULL AND value_json->>'key'=$1`, token).Scan(&value)
	if err != nil {
		return plugin.Identity{}, err
	}
	var resource struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err = json.Unmarshal(value, &resource); err != nil {
		return plugin.Identity{}, err
	}
	m := resource.Metadata
	field := func(name string) string { v, _ := m["cf.agentgateway-broker/"+name].(string); return v }
	identity := plugin.Identity{Installation: field("installation"), Instance: field("instance"), Org: field("org"), Space: field("space")}
	if identity.Instance == "" || identity.Installation == "" {
		return plugin.Identity{}, errors.New("credential has no broker identity")
	}
	return identity, nil
}

func validTime(t time.Time) bool {
	return t.UnixMilli() >= cll.MinPortableUnixMillis && t.UnixMilli() <= cll.MaxPortableUnixMillis
}

func (s *Store) appendTx(ctx context.Context, tx pgx.Tx, in cll.AppendInput) (cll.AppendResult, error) {
	if len(in.Value) != 32 || !validTime(in.AppendedAt) {
		return cll.AppendResult{}, cll.ErrInvalid
	}
	var count uint64
	if err := tx.QueryRow(ctx, "SELECT entry_count FROM agw_capsule_logs WHERE log_id=$1 FOR UPDATE", s.logID).Scan(&count); err != nil {
		return cll.AppendResult{}, err
	}
	entry, err := s.getEntry(ctx, tx, in.Value)
	if err == nil {
		return cll.AppendResult{Entry: entry, Outcome: cll.AppendIdempotent}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return cll.AppendResult{}, err
	}
	if count >= cll.MaxPortableInteger {
		return cll.AppendResult{}, cll.ErrInvalid
	}
	entry = cll.Entry{Seq: count + 1, Value: bytes.Clone(in.Value), AppendedAt: in.AppendedAt.UTC().Truncate(time.Millisecond)}
	if _, err = tx.Exec(ctx, "INSERT INTO agw_capsule_entries VALUES($1,$2,$3,$4)", s.logID, entry.Seq, entry.Value, entry.AppendedAt.UnixMilli()); err != nil {
		return cll.AppendResult{}, err
	}
	_, err = tx.Exec(ctx, "UPDATE agw_capsule_logs SET entry_count=$2 WHERE log_id=$1", s.logID, entry.Seq)
	return cll.AppendResult{Entry: entry, Outcome: cll.AppendInserted}, err
}

func (s *Store) Append(ctx context.Context, in cll.AppendInput) (cll.AppendResult, error) {
	if err := s.check(ctx); err != nil {
		return cll.AppendResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cll.AppendResult{}, err
	}
	defer tx.Rollback(ctx)
	r, err := s.appendTx(ctx, tx, in)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s *Store) getEntry(ctx context.Context, q querier, value []byte) (cll.Entry, error) {
	var e cll.Entry
	var ms int64
	err := q.QueryRow(ctx, "SELECT seq,identity,appended_ms FROM agw_capsule_entries WHERE log_id=$1 AND identity=$2", s.logID, value).Scan(&e.Seq, &e.Value, &ms)
	e.AppendedAt = time.UnixMilli(ms).UTC()
	return e, err
}
func (s *Store) GetEntry(ctx context.Context, value []byte) (cll.Entry, error) {
	if err := s.check(ctx); err != nil {
		return cll.Entry{}, err
	}
	if len(value) != 32 {
		return cll.Entry{}, cll.ErrInvalid
	}
	e, err := s.getEntry(ctx, s.pool, value)
	if errors.Is(err, pgx.ErrNoRows) {
		err = cll.ErrNotFound
	}
	return e, err
}
func (s *Store) EntryCount(ctx context.Context) (uint64, error) {
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	var count uint64
	err := s.pool.QueryRow(ctx, "SELECT entry_count FROM agw_capsule_logs WHERE log_id=$1", s.logID).Scan(&count)
	return count, err
}
func (s *Store) ScanEntries(ctx context.Context, after uint64, limit int) ([]cll.Entry, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if after > cll.MaxPortableInteger || limit < 1 || limit > cll.MaxScanLimit {
		return nil, cll.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, "SELECT seq,identity,appended_ms FROM agw_capsule_entries WHERE log_id=$1 AND seq>$2 ORDER BY seq LIMIT $3", s.logID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]cll.Entry, 0)
	for rows.Next() {
		var e cll.Entry
		var ms int64
		if err = rows.Scan(&e.Seq, &e.Value, &ms); err != nil {
			return nil, err
		}
		e.AppendedAt = time.UnixMilli(ms).UTC()
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *Store) LoadCLL(ctx context.Context) (cll.State, error) {
	if err := s.check(ctx); err != nil {
		return cll.State{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return cll.State{}, err
	}
	defer tx.Rollback(ctx)
	state, err := s.loadState(ctx, tx, false)
	if err != nil {
		return state, err
	}
	return state, tx.Commit(ctx)
}
func (s *Store) loadState(ctx context.Context, q querier, lock bool) (cll.State, error) {
	sql := "SELECT state FROM agw_capsule_logs WHERE log_id=$1"
	if lock {
		sql += " FOR UPDATE"
	}
	var raw []byte
	var state cll.State
	if err := q.QueryRow(ctx, sql, s.logID).Scan(&raw); err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("%w: state JSON", cll.ErrCorrupt)
	}
	rows, err := q.Query(ctx, "SELECT hash FROM agw_capsule_nodes WHERE log_id=$1 ORDER BY position", s.logID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var hash []byte
		if err = rows.Scan(&hash); err != nil {
			rows.Close()
			return state, err
		}
		state.Nodes = append(state.Nodes, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return state, err
	}
	rows, err = q.Query(ctx, "SELECT state FROM agw_capsule_witnesses WHERE log_id=$1 ORDER BY checkpoint_size,witness_id", s.logID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var raw []byte
		var w cll.WitnessState
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return state, err
		}
		if err = json.Unmarshal(raw, &w); err != nil {
			rows.Close()
			return state, err
		}
		state.Witnesses = append(state.Witnesses, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return state, err
	}
	return state, nil
}

// Reuse the public upstream backend as the transition validator. It preserves
// upstream append-only and witness CAS semantics without importing internals.
func validator(state cll.State) (*memory.Store, error) {
	v := memory.New()
	if err := v.CommitCLL(context.Background(), 0, nil, state); err != nil {
		return nil, err
	}
	return v, nil
}
func (s *Store) mutate(ctx context.Context, change func(*memory.Store) error) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	state, err := s.loadState(ctx, tx, true)
	if err != nil {
		return err
	}
	v, err := validator(state)
	if err != nil {
		return err
	}
	defer v.Close()
	if err = change(v); err != nil {
		return err
	}
	next, err := v.LoadCLL(ctx)
	if err != nil {
		return err
	}
	for i := len(state.Nodes); i < len(next.Nodes); i++ {
		if _, err = tx.Exec(ctx, "INSERT INTO agw_capsule_nodes VALUES($1,$2,$3)", s.logID, i, next.Nodes[i]); err != nil {
			return err
		}
	}
	type witnessKey struct {
		id   string
		size uint64
	}
	previousWitnesses := make(map[witnessKey]*cll.WitnessState, len(state.Witnesses))
	for i := range state.Witnesses {
		w := &state.Witnesses[i]
		previousWitnesses[witnessKey{w.WitnessID, w.CheckpointSize}] = w
	}
	for _, w := range next.Witnesses {
		old := previousWitnesses[witnessKey{w.WitnessID, w.CheckpointSize}]
		raw, e := json.Marshal(w)
		if e != nil {
			return e
		}
		if old != nil {
			before, _ := json.Marshal(old)
			if bytes.Equal(before, raw) {
				continue
			}
		}
		if _, err = tx.Exec(ctx, "INSERT INTO agw_capsule_witnesses VALUES($1,$2,$3,$4) ON CONFLICT(log_id,witness_id,checkpoint_size) DO UPDATE SET state=EXCLUDED.state", s.logID, w.WitnessID, w.CheckpointSize, raw); err != nil {
			return err
		}
	}
	next.Nodes = nil
	next.Witnesses = nil
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE agw_capsule_logs SET state=$2 WHERE log_id=$1", s.logID, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) CommitCLL(ctx context.Context, size uint64, expected []byte, next cll.State) error {
	return s.mutate(ctx, func(v *memory.Store) error { return v.CommitCLL(ctx, size, expected, next) })
}
func (s *Store) CommitWitness(ctx context.Context, attempts uint32, next cll.WitnessState) error {
	return s.mutate(ctx, func(v *memory.Store) error { return v.CommitWitness(ctx, attempts, next) })
}
func (s *Store) PendingWitnesses(ctx context.Context, now time.Time, limit int) ([]cll.WitnessState, error) {
	state, err := s.LoadCLL(ctx)
	if err != nil {
		return nil, err
	}
	v, err := validator(state)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	return v.PendingWitnesses(ctx, now, limit)
}
func (s *Store) GetWitness(ctx context.Context, id string, size uint64) (cll.WitnessState, error) {
	state, err := s.LoadCLL(ctx)
	if err != nil {
		return cll.WitnessState{}, err
	}
	v, err := validator(state)
	if err != nil {
		return cll.WitnessState{}, err
	}
	defer v.Close()
	return v.GetWitness(ctx, id, size)
}

var _ cll.Backend = (*Store)(nil)
var _ plugin.Repository = (*Store)(nil)
