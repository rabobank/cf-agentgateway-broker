package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"time"

	api "github.com/agentgateway/agentgateway/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type Server struct {
	api.UnimplementedExtMcpServer
	repo     Repository
	producer Producer
}

func NewServer(repo Repository, producer Producer) *Server {
	return &Server{repo: repo, producer: producer}
}

func (s *Server) CheckRequest(ctx context.Context, req *api.McpRequest) (*api.McpRequestResult, error) {
	var token string
	var authorizationHeaders int
	for _, h := range req.Headers {
		if strings.EqualFold(h.Key, "authorization") {
			authorizationHeaders++
			if authorizationHeaders > 1 {
				return nil, status.Error(codes.Unauthenticated, "ambiguous authorization")
			}
			if strings.HasPrefix(string(h.Value), "Bearer ") {
				token = strings.TrimPrefix(string(h.Value), "Bearer ")
			}
		}
	}
	var identity Identity
	var err error
	// Binding metadata originates from the loopback live-grant authorization
	// service, after frontend JWT validation. It is not a consumer header.
	if value, ok := req.MetadataContext.AsMap()["binding"]; ok {
		data, e := json.Marshal(value)
		if e != nil || json.Unmarshal(data, &identity) != nil || identity.Binding == "" || identity.Account.Issuer == "" || identity.Account.Subject == "" || len(req.ServiceNames) != 1 || identity.Target != req.ServiceNames[0] {
			return nil, status.Error(codes.Unauthenticated, "invalid binding context")
		}
	} else {
		if token == "" {
			return nil, status.Error(codes.Unauthenticated, "bearer credential required")
		}
		identity, err = s.repo.Authenticate(ctx, token)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid service-instance credential")
		}
	}
	response := &api.McpRequestResult{Result: &api.McpRequestResult_Pass{Pass: &api.Pass{}}}
	if req.Method != "tools/call" {
		return response, nil
	}
	var params struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(req.McpRequest, &params) != nil || params.Name == "" || len(req.ServiceNames) != 1 {
		return nil, status.Error(codes.InvalidArgument, "invalid tool call")
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return nil, status.Error(codes.Internal, "cannot allocate call identity")
	}
	record := Capsule{ID: hex.EncodeToString(id), Identity: identity, Phase: "planned", Tool: params.Name, Backends: req.ServiceNames, ObservedAt: time.Now().UTC()}
	if err = s.persist(ctx, &record, req.McpRequest); err != nil {
		return nil, err
	}
	response.Metadata, _ = structpb.NewStruct(map[string]any{"capsule_plan": record.ID})
	return response, nil
}

func (s *Server) CheckResponse(ctx context.Context, req *api.McpResponse) (*api.McpResponseResult, error) {
	response := &api.McpResponseResult{Result: &api.McpResponseResult_Pass{Pass: &api.Pass{}}}
	if req.Method != "tools/call" {
		return response, nil
	}
	id, ok := req.MetadataContext.AsMap()["capsule_plan"].(string)
	if !ok || id == "" {
		return nil, status.Error(codes.FailedPrecondition, "missing capsule correlation")
	}
	plan, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "planned capsule unavailable")
	}
	if plan.Phase != "planned" || !slices.Equal(plan.Backends, req.ServiceNames) {
		return nil, status.Error(codes.FailedPrecondition, "capsule correlation mismatch")
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(req.McpResponse, &result) != nil || result == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid tool result")
	}
	phase := "confirmed"
	if string(result["isError"]) == "true" {
		phase = "failed"
	}
	record := Capsule{ID: plan.ID + "/outcome", Parent: plan.ID, Identity: plan.Identity, Phase: phase, Tool: plan.Tool, Backends: plan.Backends, ObservedAt: time.Now().UTC()}
	if err = s.persist(ctx, &record, req.McpResponse); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *Server) persist(ctx context.Context, record *Capsule, body json.RawMessage) error {
	sealed, err := s.producer.Seal(*record, body)
	if err != nil {
		log.Printf("capsule creation failed: %v", err)
		return status.Error(codes.Internal, "capsule creation failed")
	}
	record.ID, record.Bytes, record.Envelope = sealed.CapsuleID, sealed.Payload, sealed.Envelope
	if err = s.repo.Save(ctx, *record); err != nil {
		log.Printf("capsule ledger commit failed capsule=%s: %v", record.ID, err)
		return status.Error(codes.Unavailable, "capsule ledger commit failed")
	}
	return nil
}
