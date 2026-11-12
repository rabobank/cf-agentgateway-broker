package postgres_test

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/postgres"
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/action-state-group/capsule-emit/go/artifact"
	artifactpg "github.com/action-state-group/capsule-emit/go/artifact/postgres"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	api "github.com/agentgateway/agentgateway/api"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPostgres(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "PostgreSQL capsule ledger") }

var pool *pgxpool.Pool
var uri string
var key ed25519.PrivateKey
var pub ed25519.PublicKey

var _ = BeforeSuite(func() {
	work, err := os.MkdirTemp("", "capsule-postgres-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, work)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port
	Expect(listener.Close()).To(Succeed())
	data := filepath.Join(work, "data")
	run := func(name string, args ...string) {
		GinkgoHelper()
		out, err := exec.Command(name, args...).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "%s: %s", name, out)
	}
	run("initdb", "-D", data, "-A", "trust", "-U", "postgres")
	run("pg_ctl", "-D", data, "-l", filepath.Join(work, "log"), "-o", fmt.Sprintf("-h 127.0.0.1 -p %d -k %s", port, work), "-w", "start")
	DeferCleanup(func() { run("pg_ctl", "-D", data, "-m", "fast", "-w", "stop") })
	uri = fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	pool, err = pgxpool.New(context.Background(), uri)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	pub, key, err = ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
})

var _ = Describe("Durable ledger", func() {
	var store *postgres.Store
	var logID string
	BeforeEach(func() {
		logID = fmt.Sprintf("test-%d", time.Now().UnixNano())
		var err error
		store, err = postgres.Open(context.Background(), pool, logID, []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(store.Close)
		Expect(store.Init(context.Background())).To(Succeed())
	})
	It("initializes concurrently with a shared one-connection pool", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cfg, err := pgxpool.ParseConfig(uri)
		Expect(err).NotTo(HaveOccurred())
		cfg.MaxConns = 1
		small, err := pgxpool.NewWithConfig(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(small.Close)
		var stores []*postgres.Store
		for i := 0; i < 4; i++ {
			current, err := postgres.Open(ctx, small, fmt.Sprintf("init-%s-%d", logID, i), []ed25519.PublicKey{pub})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(current.Close)
			stores = append(stores, current)
		}
		start := make(chan struct{})
		results := make(chan error, len(stores))
		for _, current := range stores {
			go func(s *postgres.Store) {
				<-start
				results <- s.Init(ctx)
			}(current)
		}
		close(start)
		for range stores {
			Expect(<-results).To(Succeed())
		}
		Expect(small.Ping(ctx)).To(Succeed())
		Expect(small.Stat().AcquiredConns()).To(BeZero())
	})
	It("atomically stores a signed capsule and its CLL identity", func() {
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "test", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		sealed, err := producer.Seal(record, json.RawMessage(`{"name":"echo"}`))
		Expect(err).NotTo(HaveOccurred())
		record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
		Expect(store.Save(context.Background(), record)).To(Succeed())
		Expect(store.Save(context.Background(), record)).To(Succeed())
		entries, err := store.ScanEntries(context.Background(), 0, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		loaded, err := store.Get(context.Background(), record.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.Identity).To(Equal(record.Identity))
		Expect(loaded.Bytes).To(Equal(record.Bytes))
		Expect(loaded.Envelope).To(Equal(record.Envelope))
		// Reopen on another handle and read exact signed bytes through upstream.
		db, err := sql.Open("pgx", uri)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		namespace := sha256.Sum256([]byte(logID))
		upstream, err := artifactpg.New(db, hex.EncodeToString(namespace[:]), []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		stored, err := upstream.Get(context.Background(), record.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Capsule).To(Equal(record.Bytes))
		Expect(stored.ProducerEnvelope).To(Equal(record.Envelope))
		Expect(stored.Artifacts).To(BeEmpty())
		var retained bool
		Expect(pool.QueryRow(context.Background(), "SELECT record->>'Bytes' IS NOT NULL OR record->>'Envelope' IS NOT NULL FROM agw_capsules WHERE log_id=$1 AND capsule_id=$2", logID, record.ID).Scan(&retained)).To(Succeed())
		Expect(retained).To(BeFalse())
		signer, err := checkpoint.NewEd25519Signer(key)
		Expect(err).NotTo(HaveOccurred())
		cfg := checkpoint.DefaultRunnerConfig(logID)
		cfg.Cadence.CadenceEntries = 1
		cfg.WitnessIDs = []string{"test-witness"}
		runner, err := checkpoint.NewRunner(cfg, store, signer)
		Expect(err).NotTo(HaveOccurred())
		_, err = runner.RunOnce(context.Background(), time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		state, err := store.LoadCLL(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Checkpoint).NotTo(BeNil())
		parsed, err := checkpoint.ParseRecord(state.Checkpoint.Bytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.VerifySignature()).To(Succeed())
		tree, err := mmr.New(state.Nodes)
		Expect(err).NotTo(HaveOccurred())
		proof, err := tree.InclusionProof(0, state.Checkpoint.Size)
		Expect(err).NotTo(HaveOccurred())
		root, err := hex.DecodeString(parsed.Root)
		Expect(err).NotTo(HaveOccurred())
		Expect(mmr.VerifyInclusion(root, state.Checkpoint.Size, 0, entries[0].Value, proof)).To(BeTrue())
		pending, err := store.PendingWitnesses(context.Background(), time.Now().UTC(), 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(HaveLen(1))
	})
	It("persists signed capsules with full, partial and absent identity attribution", func() {
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		for i, attribution := range []string{
			`{"service_account":{"issuer":"https://issuer.test","subject":"account"},"selected_identity":{"backend_id":"tools","auth_method":"federated_client_credentials","client_id":"client","authority":"https://tokens.test"}}`,
			`{"selected_identity":{"auth_method":"passthrough"}}`,
			`{}`,
		} {
			identity := plugin.Identity{Installation: logID, Instance: "one"}
			Expect(json.Unmarshal([]byte(attribution), &identity)).To(Succeed())
			record := plugin.Capsule{ID: fmt.Sprintf("attribution-%d", i), Identity: identity, Phase: "planned", Tool: "echo", Backends: []string{"target"}, ObservedAt: time.Now().UTC()}
			sealed, err := producer.Seal(record, json.RawMessage(`{"name":"echo"}`))
			Expect(err).NotTo(HaveOccurred())
			record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
			Expect(store.Save(context.Background(), record)).To(Succeed())
			Expect(store.Save(context.Background(), record)).To(Succeed())
			loaded, err := store.Get(context.Background(), record.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Identity).To(Equal(identity))
			for _, altered := range []string{
				`{"service_account":{"issuer":"https://other.test","subject":"different"}}`,
				`{"selected_identity":{"client_id":"other-client"}}`,
			} {
				changed := record
				// Decode a fresh value so this test does not share nested objects.
				data, err := json.Marshal(identity)
				Expect(err).NotTo(HaveOccurred())
				Expect(json.Unmarshal(data, &changed.Identity)).To(Succeed())
				Expect(json.Unmarshal([]byte(altered), &changed.Identity)).To(Succeed())
				Expect(store.Save(context.Background(), changed)).NotTo(Succeed())
			}
		}
	})
	It("authorizes only the live binding account and denies tombstoned instances and bindings", func() {
		ctx := context.Background()
		// Fixture for the gateway-owned store; the sidecar never creates this in production.
		_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS agw_config_resources (kind text,id text,value_json jsonb,deleted_at timestamptz,PRIMARY KEY(kind,id))`)
		Expect(err).NotTo(HaveOccurred())
		grant := mcpconfig.Binding{ID: "binding", Instance: mcpconfig.Instance{ID: "instance", Installation: logID, Backend: mcpconfig.Backend{Name: "tools", Auth: mcpconfig.Auth{Method: "passthrough"}}}, Account: mcpconfig.Account{Issuer: "https://issuer.test", Subject: "account"}}
		parentID := mcpconfig.ManagedID(logID, "instance", "")
		grantID := mcpconfig.ManagedID(logID, "instance", "binding")
		instance, _ := json.Marshal(map[string]any{"name": parentID, "metadata": grant.Instance})
		binding, _ := json.Marshal(map[string]any{"name": grantID, "instance": parentID, "metadata": grant})
		_, err = pool.Exec(ctx, "INSERT INTO agw_config_resources VALUES('mcp.instance',$1,$2,NULL)", parentID, instance)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { pool.Exec(ctx, "DELETE FROM agw_config_resources WHERE id IN ($1,$2)", parentID, grantID) })
		_, err = pool.Exec(ctx, "INSERT INTO agw_config_resources VALUES('mcp.grant',$1,$2,NULL)", grantID, binding)
		Expect(err).NotTo(HaveOccurred())
		identity, err := store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.SelectedIdentity.BackendID).To(Equal("tools"))
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "wrong-account")
		Expect(err).To(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=now() WHERE kind='mcp.grant' AND id=$1`, grantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).To(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=NULL WHERE kind='mcp.grant' AND id=$1`, grantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=now() WHERE kind='mcp.instance' AND id=$1`, parentID)
		Expect(err).NotTo(HaveOccurred())
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).To(HaveOccurred())
	})
	It("authorizes the stable managed grant ID rather than matching unrelated metadata", func() {
		ctx := context.Background()
		_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS agw_config_resources (kind text,id text,value_json jsonb,deleted_at timestamptz,PRIMARY KEY(kind,id))`)
		Expect(err).NotTo(HaveOccurred())
		id := func(instance, binding string) string {
			data, e := json.Marshal([]string{logID, instance, binding})
			Expect(e).NotTo(HaveOccurred())
			digest := sha256.Sum256(data)
			return "cf-mcp-" + hex.EncodeToString(digest[:])
		}
		parentID, grantID := id("instance", ""), id("instance", "binding")
		decoyID := logID + "-decoy"
		grant := mcpconfig.Binding{ID: "binding", Instance: mcpconfig.Instance{ID: "instance", Installation: logID}, Account: mcpconfig.Account{Issuer: "https://issuer.test", Subject: "account"}}
		parent, _ := json.Marshal(map[string]any{"name": parentID, "metadata": grant.Instance})
		value, _ := json.Marshal(map[string]any{"name": grantID, "instance": parentID, "metadata": grant})
		_, err = pool.Exec(ctx, `INSERT INTO agw_config_resources VALUES('mcp.instance',$1,$2,NULL),('mcp.grant',$3,$4,NULL)`, parentID, parent, decoyID, value)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			pool.Exec(ctx, "DELETE FROM agw_config_resources WHERE id IN ($1,$2,$3)", parentID, grantID, decoyID)
		})
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).To(HaveOccurred(), "metadata alone must not select a grant stored under another resource ID")
		_, err = pool.Exec(ctx, `INSERT INTO agw_config_resources VALUES('mcp.grant',$1,$2,NULL)`, grantID, value)
		Expect(err).NotTo(HaveOccurred())
		identity, err := store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.Binding).To(Equal("binding"))
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=now() WHERE kind='mcp.instance' AND id=$1`, parentID)
		Expect(err).NotTo(HaveOccurred())
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).To(HaveOccurred())
	})
	It("does not persist anything when evidence verification fails", func() {
		Expect(store.Save(context.Background(), plugin.Capsule{ID: "invalid", Bytes: []byte(`{}`)})).NotTo(Succeed())
		entries, err := store.ScanEntries(context.Background(), 0, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})
	It("rolls back a CLL append when the capsule insert fails", func() {
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "rollback", Identity: plugin.Identity{Installation: "lab", Instance: "rollback"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		sealed, err := producer.Seal(record, json.RawMessage(`{"name":"echo"}`))
		Expect(err).NotTo(HaveOccurred())
		record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
		_, err = pool.Exec(context.Background(), `ALTER TABLE agw_capsules ADD CONSTRAINT test_reject_rollback CHECK(instance_id <> 'rollback')`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, e := pool.Exec(context.Background(), "ALTER TABLE agw_capsules DROP CONSTRAINT test_reject_rollback")
			Expect(e).NotTo(HaveOccurred())
		})
		Expect(store.Save(context.Background(), record)).NotTo(Succeed())
		entries, err := store.ScanEntries(context.Background(), 0, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
		count, err := store.EntryCount(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero())
		Expect(pool.QueryRow(context.Background(), "SELECT count(*) FROM capsule_store_capsules WHERE capsule_id=$1", record.ID).Scan(&count)).To(Succeed())
		Expect(count).To(BeZero(), "artifact insertion must roll back with the application index and CLL")
	})
	It("rolls back application and CLL writes when upstream rejects a conflicting artifact", func() {
		ctx := context.Background()
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "conflict", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		sealed, err := producer.Seal(record, json.RawMessage(`{}`))
		Expect(err).NotTo(HaveOccurred())
		record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
		db, err := sql.Open("pgx", uri)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		namespace := sha256.Sum256([]byte(logID))
		upstream, err := artifactpg.New(db, hex.EncodeToString(namespace[:]), []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		Expect(upstream.Put(ctx, artifact.Record{CapsuleID: record.ID, Capsule: record.Bytes, ProducerEnvelope: record.Envelope, Artifacts: []artifact.Artifact{{Name: "payload", Binding: artifact.PayloadDigest, State: artifact.NeverRetained}}})).To(Succeed())
		Expect(store.Save(ctx, record)).To(MatchError(artifact.ErrConflict))
		count, err := store.EntryCount(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero())
		var rows int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM agw_capsules WHERE log_id=$1", logID).Scan(&rows)).To(Succeed())
		Expect(rows).To(BeZero())
	})
	It("fails closed on upstream artifact corruption", func() {
		ctx := context.Background()
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "corrupt", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		sealed, err := producer.Seal(record, json.RawMessage(`{}`))
		Expect(err).NotTo(HaveOccurred())
		record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
		Expect(store.Save(ctx, record)).To(Succeed())
		_, err = pool.Exec(ctx, "UPDATE capsule_store_capsules SET capsule_bytes=$2 WHERE capsule_id=$1", record.ID, []byte(`{}`))
		Expect(err).NotTo(HaveOccurred())
		loaded, err := store.Get(ctx, record.ID)
		Expect(err).To(HaveOccurred())
		Expect(loaded).To(BeZero())
	})
	It("allocates dense sequence numbers across independent handles", func() {
		other, err := postgres.Open(context.Background(), pool, logID, []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(other.Close)
		errors := make(chan error, 20)
		for i := 0; i < 20; i++ {
			go func(i int) {
				value := make([]byte, 32)
				value[0] = byte(i)
				s := store
				if i%2 == 0 {
					s = other
				}
				_, err := s.Append(context.Background(), cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
				errors <- err
			}(i)
		}
		for i := 0; i < 20; i++ {
			Expect(<-errors).NotTo(HaveOccurred())
		}
		entries, err := store.ScanEntries(context.Background(), 0, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(20))
		for i, entry := range entries {
			Expect(entry.Seq).To(Equal(uint64(i + 1)))
		}
	})
	It("serializes concurrent byte-identical saves across sidecars without duplicate CLL entries", func() {
		ctx := context.Background()
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "retry", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		sealed, err := producer.Seal(record, json.RawMessage(`{}`))
		Expect(err).NotTo(HaveOccurred())
		record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
		other, err := postgres.Open(ctx, pool, logID, []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(other.Close)
		results := make(chan error, 12)
		for i := 0; i < cap(results); i++ {
			current := store
			if i%2 == 0 {
				current = other
			}
			go func(s *postgres.Store) { results <- s.Save(ctx, record) }(current)
		}
		for i := 0; i < cap(results); i++ {
			Expect(<-results).To(Succeed())
		}
		count, err := other.EntryCount(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(uint64(1)))
		loaded, err := other.Get(ctx, record.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.Bytes).To(Equal(record.Bytes))
	})
	It("records overlapping outcomes on a different sidecar without mispairing", func() {
		ctx := context.Background()
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		other, err := postgres.Open(ctx, pool, logID, []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(other.Close)
		server := plugin.NewServer(other, producer)
		plans := make([]string, 12)
		for i := range plans {
			record := plugin.Capsule{ID: fmt.Sprintf("parallel-%d", i), Identity: plugin.Identity{Installation: "lab", Instance: fmt.Sprintf("instance-%d", i)}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
			sealed, e := producer.Seal(record, json.RawMessage(`{"name":"echo"}`))
			Expect(e).NotTo(HaveOccurred())
			record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
			Expect(store.Save(ctx, record)).To(Succeed())
			plans[i] = record.ID
		}
		results := make(chan error, len(plans))
		for _, id := range plans {
			go func(id string) {
				md, _ := structpb.NewStruct(map[string]any{"capsule_plan": id})
				_, e := server.CheckResponse(ctx, &api.McpResponse{Method: "tools/call", ServiceNames: []string{"test"}, MetadataContext: md, McpResponse: []byte(`{"content":[]}`)})
				results <- e
			}(id)
		}
		for range plans {
			Expect(<-results).NotTo(HaveOccurred())
		}
		var count int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM agw_capsules WHERE log_id=$1 AND parent_id IS NOT NULL", logID).Scan(&count)).To(Succeed())
		Expect(count).To(Equal(len(plans)))
	})
})
