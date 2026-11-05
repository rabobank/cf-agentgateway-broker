package plugin_test

import (
	"cf-agentgateway-capsule/plugin"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	emit "github.com/action-state-group/capsule-emit/go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"time"
)

var _ = Describe("Signed capsules", func() {
	It("losslessly commits decimals and arbitrary JSON integers in requests and results", func() {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		producer, err := plugin.NewProducer(key)
		Expect(err).NotTo(HaveOccurred())
		body := json.RawMessage(`{"temperature":0.5,"large":9007199254740993,"exponent":1e100,"negativeZero":-0.0,"string":"0.5"}`)
		wrapper := map[string]string{"encoding": "mcp-json-bytes/base64/v1", "data": base64.StdEncoding.EncodeToString(body)}
		digest, err := emit.DigestJSON(wrapper)
		Expect(err).NotTo(HaveOccurred())
		record := plugin.Capsule{ID: "numbers", Identity: plugin.Identity{Installation: "lab", Instance: "one"}, Phase: "planned", Tool: "echo", Backends: []string{"test"}, ObservedAt: time.Now().UTC()}
		plan, err := producer.Seal(record, body)
		Expect(err).NotTo(HaveOccurred())
		var content map[string]any
		Expect(json.Unmarshal(plan.Payload, &content)).To(Succeed())
		compute := content["model_attestation"].(map[string]any)["compute_attestation"].(map[string]any)
		Expect(compute["agent_input_digest"]).To(Equal(digest))
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
		decoded, err := base64.StdEncoding.DecodeString(wrapper["data"])
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(Equal([]byte(body)))
		changed, err := producer.Seal(record, json.RawMessage(`{"temperature":"0.5"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(changed.CapsuleID).NotTo(Equal(outcome.CapsuleID))
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
