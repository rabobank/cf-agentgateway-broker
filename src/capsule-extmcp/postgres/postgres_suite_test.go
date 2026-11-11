package postgres_test

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/postgres"
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		Expect(store.Init(context.Background())).To(Succeed())
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
	It("authorizes only the live binding account and denies tombstoned instances and bindings", func() {
		ctx := context.Background()
		// Fixture for the gateway-owned store; the sidecar never creates this in production.
		_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS agw_config_resources (kind text,id text,value_json jsonb,deleted_at timestamptz,PRIMARY KEY(kind,id))`)
		Expect(err).NotTo(HaveOccurred())
		grant := mcpconfig.Binding{ID: "binding", Instance: mcpconfig.Instance{ID: "instance", Installation: logID, Backend: mcpconfig.Backend{Name: "tools", Auth: mcpconfig.Auth{Method: "passthrough"}}}, Account: mcpconfig.Account{Issuer: "https://issuer.test", Subject: "account"}}
		instance, _ := json.Marshal(map[string]any{"name": logID, "metadata": grant.Instance})
		binding, _ := json.Marshal(map[string]any{"name": logID, "instance": logID, "metadata": grant})
		_, err = pool.Exec(ctx, "INSERT INTO agw_config_resources VALUES('mcp.instance',$1,$2,NULL)", logID, instance)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { pool.Exec(ctx, "DELETE FROM agw_config_resources WHERE id=$1", logID) })
		_, err = pool.Exec(ctx, "INSERT INTO agw_config_resources VALUES('mcp.grant',$1,$2,NULL)", logID, binding)
		Expect(err).NotTo(HaveOccurred())
		identity, err := store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.SelectedIdentity.BackendID).To(Equal("tools"))
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "wrong-account")
		Expect(err).To(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=now() WHERE kind='mcp.grant' AND id=$1`, logID)
		Expect(err).NotTo(HaveOccurred())
		_, err = store.AuthorizeBinding(ctx, "instance", "binding", "https://issuer.test", "account")
		Expect(err).To(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=NULL WHERE kind='mcp.grant' AND id=$1`, logID)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agw_config_resources SET deleted_at=now() WHERE kind='mcp.instance' AND id=$1`, logID)
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
	})
	It("allocates dense sequence numbers across independent handles", func() {
		other, err := postgres.Open(context.Background(), pool, logID, []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
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
	It("records overlapping outcomes on a different sidecar without mispairing", func() {
		ctx := context.Background()
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		other, err := postgres.Open(ctx, pool, logID, []ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
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
