package plugin_test

import (
	"cf-agentgateway-capsule/plugin"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	emit "github.com/action-state-group/capsule-emit/go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"time"
)

var _ = Describe("Signed capsules", func() {
	It("commits upstream-canonicalized numbers in requests and results", func() {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		body := json.RawMessage(`{"temperature":0.5,"large":9007199254740991,"nested":[1e21,-0.0,1.0,1]}`)
		digest, err := emit.DigestJSON(json.RawMessage(`{"temperature":"0.5","large":9007199254740991,"nested":["1e+21","0","1",1]}`))
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "numbers", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		plan, err := producer.Seal(record, body)
		Expect(err).NotTo(HaveOccurred())
		var content map[string]any
		Expect(json.Unmarshal(plan.Payload, &content)).To(Succeed())
		compute := content["model_attestation"].(map[string]any)["compute_attestation"].(map[string]any)
		Expect(compute["agent_input_digest"]).To(Equal(digest))
		Expect(content["gateway"].(map[string]any)["body_encoding"]).To(Equal("mcp-json/canonicalize-for-digest/v1"))
		record.ID = "numbers/outcome"
		record.Parent = plan.CapsuleID
		record.Phase = "confirmed"
		outcome, err := producer.Seal(record, body)
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(outcome.Payload, &content)).To(Succeed())
		compute = content["model_attestation"].(map[string]any)["compute_attestation"].(map[string]any)
		Expect(compute["agent_output_digest"]).To(Equal(digest))
		Expect(content["effect"].(map[string]any)["response_digest"]).To(Equal(digest))
		check, err := emit.VerifyCapsule(outcome.Payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.OK).To(BeTrue())
		for _, changedBody := range []string{
			`{"temperature":"0.5","large":9007199254740991,"nested":[1e21,-0.0,1.0,1]}`,
			`{"temperature":0.50,"large":9007199254740991,"nested":[1e21,-0.0,1.0,1]}`,
			`{ "nested":[1e21,-0.0,1.0,1], "large":9007199254740991,"temperature":0.5 }`,
		} {
			changed, err := producer.Seal(record, json.RawMessage(changedBody))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed.CapsuleID).To(Equal(outcome.CapsuleID))
		}
	})
	It("rejects unsafe integer tokens, overflowing numbers and invalid JSON", func() {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "invalid", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		for _, body := range []string{
			`{"large":9007199254740993}`, // UseNumber must prevent rounding into a float.
			`{"large":-9007199254740992}`,
			`{"nested":[1e999]}`,
			`{"temperature":`,
			`{} {}`,
		} {
			sealed, err := producer.Seal(record, json.RawMessage(body))
			Expect(err).To(HaveOccurred(), body)
			Expect(sealed).To(BeZero())
		}
	})
	It("includes a digest for JSON null bodies", func() {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "null", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		sealed, err := producer.Seal(record, json.RawMessage(`null`))
		Expect(err).NotTo(HaveOccurred())
		digest, err := emit.DigestJSON(json.RawMessage(`null`))
		Expect(err).NotTo(HaveOccurred())
		var content map[string]any
		Expect(json.Unmarshal(sealed.Payload, &content)).To(Succeed())
		compute := content["model_attestation"].(map[string]any)["compute_attestation"].(map[string]any)
		Expect(compute["agent_input_digest"]).To(Equal(digest))
	})
	It("commits the service instance and links a verifiable outcome", func() {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "call-1", Identity: plugin.Identity{Installation: "lab", Instance: "instance-a"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		plan, err := producer.Seal(record, json.RawMessage(`{"name":"echo"}`))
		Expect(err).NotTo(HaveOccurred())
		check, err := emit.VerifyCapsule(plan.Payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.OK).To(BeTrue())
		envelope, err := emit.VerifyEnvelope(plan.CapsuleID, plan.Envelope)
		Expect(err).NotTo(HaveOccurred())
		Expect(envelope.PublicKey).To(Equal([]byte(pub)))
		var content map[string]any
		Expect(json.Unmarshal(plan.Payload, &content)).To(Succeed())
		Expect(content["gateway"].(map[string]any)["service_instance_id"]).To(Equal("instance-a"))
		record.Parent = plan.CapsuleID
		record.Phase = "confirmed"
		record.ID = "call-1/outcome"
		outcome, err := producer.Seal(record, json.RawMessage(`{"content":[]}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(outcome.Payload, &content)).To(Succeed())
		Expect(content["chain"].(map[string]any)["parent_capsule_id"]).To(Equal(plan.CapsuleID))
	})
})
