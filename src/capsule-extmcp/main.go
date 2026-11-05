package main

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/postgres"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	api "github.com/agentgateway/agentgateway/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func required(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}
func databaseURL() (string, error) {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v, nil
	}
	name := os.Getenv("DATABASE_SERVICE_NAME")
	if name == "" {
		name = "agentgateway-state"
	}
	var bindings map[string][]struct {
		Name        string
		Credentials map[string]any
	}
	if err := json.Unmarshal([]byte(os.Getenv("VCAP_SERVICES")), &bindings); err != nil {
		return "", err
	}
	for _, group := range bindings {
		for _, binding := range group {
			if binding.Name == name {
				for _, field := range []string{"uri", "url"} {
					if uri, ok := binding.Credentials[field].(string); ok && uri != "" {
						return uri, nil
					}
				}
			}
		}
	}
	return "", errors.New("PostgreSQL binding required")
}

func run(ctx context.Context) error {
	uri, err := databaseURL()
	if err != nil {
		return err
	}
	logID, err := required("CAPSULE_LOG_ID")
	if err != nil {
		return err
	}
	seedHex, err := required("CAPSULE_SIGNING_SEED")
	if err != nil {
		return err
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("CAPSULE_SIGNING_SEED must be a 32-byte hex seed")
	}
	key := ed25519.NewKeyFromSeed(seed)
	cfg, err := pgxpool.ParseConfig(uri)
	if err != nil {
		return err
	}
	cfg.MaxConns = 6
	cfg.MinConns = 0
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := postgres.Open(ctx, pool, logID, []ed25519.PublicKey{key.Public().(ed25519.PublicKey)})
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		return err
	}
	producer, err := plugin.NewProducer(key)
	if err != nil {
		return err
	}
	grpcAddr := os.Getenv("CAPSULE_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = "127.0.0.1:50051"
	}
	authAddr := os.Getenv("CAPSULE_AUTH_ADDR")
	if authAddr == "" {
		authAddr = "127.0.0.1:50052"
	}
	grpcListener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return err
	}
	defer grpcListener.Close()
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(8 << 20))
	api.RegisterExtMcpServer(grpcServer, plugin.NewServer(store, producer))
	mux := http.NewServeMux()
	mux.Handle("/authorize", plugin.AuthHandler(store))
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if pool.Ping(c) != nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	authServer := &http.Server{Addr: authAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- grpcServer.Serve(grpcListener) }()
	go func() { errorsCh <- authServer.ListenAndServe() }()
	leaderDone := make(chan struct{})
	go func() { defer close(leaderDone); errorsCh <- lead(child, pool, store, logID, key) }()
	log.Printf("capsule sidecar ready log=%s grpc=%s", logID, grpcAddr)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = authServer.Shutdown(shutdown)
	done := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
		grpcServer.Stop()
	}
	<-leaderDone
	return err
}

func lead(ctx context.Context, pool *pgxpool.Pool, store *postgres.Store, logID string, key ed25519.PrivateKey) error {
	signer, err := checkpoint.NewEd25519Signer(key)
	if err != nil {
		return err
	}
	config := checkpoint.DefaultRunnerConfig(logID)
	config.PollInterval = 5 * time.Second
	if raw := os.Getenv("CAPSULE_CHECKPOINT_ENTRIES"); raw != "" {
		n, e := strconv.ParseUint(raw, 10, 64)
		if e != nil {
			return e
		}
		config.Cadence.CadenceEntries = n
	}
	var delivery *witness.DeliveryRunner
	if url := os.Getenv("CAPSULE_WITNESS_URL"); url != "" {
		pub, err := hex.DecodeString(os.Getenv("CAPSULE_WITNESS_PUBLIC_KEY"))
		if err != nil || len(pub) != 32 {
			return errors.New("CAPSULE_WITNESS_PUBLIC_KEY must be 32-byte hex")
		}
		client, err := witness.NewClient(url, nil, 0)
		if err != nil {
			return err
		}
		verify, err := witness.NewReceiptVerifier(ed25519.PublicKey(pub))
		if err != nil {
			return err
		}
		config.WitnessIDs = []string{"configured-witness"}
		delivery, err = witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), store, map[string]witness.Submitter{"configured-witness": client}, map[string]witness.Verifier{"configured-witness": verify})
		if err != nil {
			return err
		}
	}
	runner, err := checkpoint.NewRunner(config, store, signer)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		lease, err := postgres.TryLeadership(ctx, pool, logID)
		if err != nil {
			log.Printf("leader acquisition failed: %v", err)
		}
		if lease != nil {
			log.Printf("checkpoint leadership acquired log=%s", logID)
			err = runLeader(ctx, lease, runner, delivery)
			_ = lease.Close()
			if err != nil && ctx.Err() == nil {
				log.Printf("checkpoint leadership ended: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

func runLeader(ctx context.Context, lease *postgres.Leadership, runner *checkpoint.Runner, delivery *witness.DeliveryRunner) error {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	workerErrors := make(chan error, 2)
	workers := 1
	go func() { workerErrors <- runner.Run(child) }()
	if delivery != nil {
		workers++
		go func() { workerErrors <- delivery.Run(child) }()
	}
	// Cancel workers before releasing the leader session. In-flight HTTP may
	// still be accepted, hence durable at-least-once delivery remains essential.
	defer func() {
		cancel()
		for i := 0; i < workers; i++ {
			<-workerErrors
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-workerErrors:
			workers--
			return err
		case <-ticker.C:
			ping, c := context.WithTimeout(ctx, 2*time.Second)
			err := lease.Heartbeat(ping)
			c()
			if err != nil {
				return err
			}
		}
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}
