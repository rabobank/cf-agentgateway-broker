package postgres_test

import (
	"cf-agentgateway-capsule/postgres"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/fxamacker/cbor/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/veraison/go-cose"
)

var _ = Describe("Witness receipts", func() {
	It("retries a temporary failure and persists a cryptographically verified response", func() {
		ctx := context.Background()
		store, err := postgres.Open(ctx, pool, fmt.Sprintf("witness-%d", time.Now().UnixNano()), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Init(ctx)).To(Succeed())
		_, err = store.Append(ctx, cll.AppendInput{Value: make([]byte, 32), AppendedAt: time.Now().UTC()})
		Expect(err).NotTo(HaveOccurred())
		signer, err := checkpoint.NewEd25519Signer(key)
		Expect(err).NotTo(HaveOccurred())
		cfg := checkpoint.DefaultRunnerConfig(store.LogID())
		cfg.Cadence.CadenceEntries = 1
		cfg.WitnessIDs = []string{"fixture"}
		runner, err := checkpoint.NewRunner(cfg, store, signer)
		Expect(err).NotTo(HaveOccurred())
		_, err = runner.RunOnce(ctx, time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		witnessPub, witnessKey, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		attempt := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempt++
			if attempt == 1 {
				w.WriteHeader(503)
				return
			}
			statement, e := io.ReadAll(r.Body)
			if e != nil {
				w.WriteHeader(400)
				return
			}
			parsed, e := checkpoint.ParseRecord(statement)
			if e != nil {
				w.WriteHeader(400)
				return
			}
			entry, e := parsed.EntryHash()
			if e != nil {
				w.WriteHeader(400)
				return
			}
			proof, e := cbor.Marshal([]any{int64(1), int64(0), [][]byte{}})
			if e != nil {
				w.WriteHeader(500)
				return
			}
			leaf := sha256.Sum256(append([]byte{0}, entry...))
			message := cose.NewSign1Message()
			message.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
			message.Headers.Protected[int64(395)] = int64(1)
			message.Headers.Unprotected[int64(396)] = map[any]any{int64(-1): []any{proof}}
			message.Payload = leaf[:]
			signer, e := cose.NewSigner(cose.AlgorithmEdDSA, witnessKey)
			if e != nil {
				w.WriteHeader(500)
				return
			}
			if message.Sign(rand.Reader, nil, signer) != nil {
				w.WriteHeader(500)
				return
			}
			message.Payload = nil
			receipt, e := message.MarshalCBOR()
			if e != nil {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt_b64": base64.StdEncoding.EncodeToString(receipt), "entry_hash": hex.EncodeToString(entry), "entry_hash_scheme": "legacy", "leaf_index": 0, "tree_size": 1})
		}))
		DeferCleanup(server.Close)
		client, err := witness.NewClient(server.URL, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		verifier, err := witness.NewReceiptVerifier(witnessPub)
		Expect(err).NotTo(HaveOccurred())
		dcfg := witness.DefaultDeliveryConfig()
		dcfg.Jitter = false
		delivery, err := witness.NewDeliveryRunner(dcfg, store, map[string]witness.Submitter{"fixture": client}, map[string]witness.Verifier{"fixture": verifier})
		Expect(err).NotTo(HaveOccurred())
		now := time.Now().UTC()
		n, err := delivery.RunOnce(ctx, now, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		state, err := store.LoadCLL(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Witnesses[0].Attempts).To(Equal(uint32(1)))
		n, err = delivery.RunOnce(ctx, now.Add(2*time.Second), 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		state, err = store.LoadCLL(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Witnesses[0].Receipt).NotTo(BeNil())
		Expect(verifier.Verify(state.Witnesses[0].Checkpoint, witness.Receipt{Bytes: state.Witnesses[0].Receipt.Bytes, EntryHash: state.Witnesses[0].Receipt.EntryHash, EntryHashScheme: "legacy", LeafIndex: 0, TreeSize: 1})).To(Succeed())
	})
})
