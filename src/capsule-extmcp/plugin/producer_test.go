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
