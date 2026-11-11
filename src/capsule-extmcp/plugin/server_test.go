package plugin_test

import (
	"cf-agentgateway-capsule/plugin"
	"cf-agentgateway-capsule/plugin/pluginfakes"
	"context"
	"encoding/json"
	"errors"
	emit "github.com/action-state-group/capsule-emit/go"
	api "github.com/agentgateway/agentgateway/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"
)

var _ = Describe("ExtMcp", func() {
	var repo *pluginfakes.FakeRepository
	var producer *pluginfakes.FakeProducer
	var server *plugin.Server
	var identity plugin.Identity
	var request *api.McpRequest
	BeforeEach(func() {
		repo = &pluginfakes.FakeRepository{}
		producer = &pluginfakes.FakeProducer{}
		identity = plugin.Identity{Installation: "test", Instance: "instance-a"}
		repo.AuthenticateReturns(identity, nil)
		producer.SealReturns(emit.Result{CapsuleID: "planned-id", Payload: []byte(`{}`), Envelope: []byte("signed")}, nil)
		server = plugin.NewServer(repo, producer)
		request = &api.McpRequest{Method: "tools/call", ServiceNames: []string{"echo"}, McpRequest: []byte(`{"name":"echo","arguments":{"text":"hi"}}`), Headers: []*api.McpHeader{{Key: "authorization", Value: []byte("Bearer secret")}}}
	})
	It("persists a plan before returning its correlation metadata", func() {
		response, err := server.CheckRequest(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.GetPass()).NotTo(BeNil())
		Expect(response.Metadata.AsMap()["capsule_plan"]).To(Equal("planned-id"))
		Expect(repo.SaveCallCount()).To(Equal(1))
		_, record := repo.SaveArgsForCall(0)
		Expect(record.Identity).To(Equal(identity))
		Expect(record.Phase).To(Equal("planned"))
	})
	It("preserves selected backend identity in the plan without claiming authentication", func() {
		var selected plugin.Identity
		Expect(json.Unmarshal([]byte(`{"installation":"test","service_instance_id":"instance-a","selected_identity":{"backend_id":"echo","auth_method":"static","identity_reference":"operator-tools"}}`), &selected)).To(Succeed())
		repo.AuthenticateReturns(selected, nil)
		_, err := server.CheckRequest(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		_, record := repo.SaveArgsForCall(0)
		data, err := json.Marshal(record.Identity)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring(`"selected_identity"`))
		Expect(string(data)).To(ContainSubstring(`"identity_reference":"operator-tools"`))
	})
	It("authenticates discovery without recording capsules", func() {
		request.Method = "tools/list"
		_, err := server.CheckRequest(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.AuthenticateCallCount()).To(Equal(1))
		Expect(repo.SaveCallCount()).To(BeZero())
	})
	It("accepts authorized binding context without selected identity attribution", func() {
		request.Headers = nil
		request.MetadataContext, _ = structpb.NewStruct(map[string]any{"binding": map[string]any{
			"installation": "test", "service_instance_id": "instance-a", "binding_id": "binding", "target": "echo",
			"service_account": map[string]any{"issuer": "https://issuer.test", "subject": "account"},
		}})
		_, err := server.CheckRequest(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.AuthenticateCallCount()).To(BeZero())
		_, record := repo.SaveArgsForCall(0)
		Expect(record.Identity.Account.Subject).To(Equal("account"))
		Expect(record.Identity.SelectedIdentity.BackendID).To(BeEmpty())
	})
	It("rejects binding context without its required service account", func() {
		request.Headers = nil
		request.MetadataContext, _ = structpb.NewStruct(map[string]any{"binding": map[string]any{
			"installation": "test", "service_instance_id": "instance-a", "binding_id": "binding", "target": "echo",
		}})
		_, err := server.CheckRequest(context.Background(), request)
		Expect(err).To(HaveOccurred())
		Expect(repo.SaveCallCount()).To(BeZero())
	})
	It("rejects missing credentials", func() {
		request.Headers = nil
		_, err := server.CheckRequest(context.Background(), request)
		Expect(err).To(HaveOccurred())
		Expect(repo.SaveCallCount()).To(BeZero())
	})
	It("fails closed when the ledger commit fails", func() {
		repo.SaveReturns(errors.New("database unavailable"))
		_, err := server.CheckRequest(context.Background(), request)
		Expect(err).To(HaveOccurred())
	})
	It("correlates a response using a persisted plan rather than local state", func() {
		repo.GetReturns(plugin.Capsule{ID: "plan-b", Identity: identity, Phase: "planned", Tool: "echo", Backends: []string{"echo"}}, nil)
		metadata, _ := structpb.NewStruct(map[string]any{"capsule_plan": "plan-b"})
		_, err := server.CheckResponse(context.Background(), &api.McpResponse{Method: "tools/call", ServiceNames: []string{"echo"}, MetadataContext: metadata, McpResponse: []byte(`{"content":[],"isError":true}`)})
		Expect(err).NotTo(HaveOccurred())
		_, record := repo.SaveArgsForCall(0)
		Expect(record.Parent).To(Equal("plan-b"))
		Expect(record.Identity).To(Equal(identity))
		Expect(record.Phase).To(Equal("failed"))
	})
	It("rejects outcomes without a correlation identifier", func() {
		_, err := server.CheckResponse(context.Background(), &api.McpResponse{Method: "tools/call"})
		Expect(err).To(HaveOccurred())
	})
	It("rejects an outcome routed to a different backend", func() {
		repo.GetReturns(plugin.Capsule{ID: "plan", Identity: identity, Phase: "planned", Backends: []string{"first"}}, nil)
		metadata, _ := structpb.NewStruct(map[string]any{"capsule_plan": "plan"})
		_, err := server.CheckResponse(context.Background(), &api.McpResponse{Method: "tools/call", ServiceNames: []string{"other"}, MetadataContext: metadata, McpResponse: []byte(`{}`)})
		Expect(err).To(HaveOccurred())
		Expect(repo.SaveCallCount()).To(BeZero())
	})
})
