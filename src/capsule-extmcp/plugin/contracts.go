package plugin

import (
	"context"
	"encoding/json"
	"time"

	emit "github.com/action-state-group/capsule-emit/go"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate . Repository
type Repository interface {
	Authenticate(context.Context, string) (Identity, error)
	Save(context.Context, Capsule) error
	Get(context.Context, string) (Capsule, error)
}

type Identity struct {
	Installation string `json:"installation"`
	Instance     string `json:"service_instance_id"`
	Org          string `json:"org_id,omitempty"`
	Space        string `json:"space_id,omitempty"`
}

type Capsule struct {
	ID         string
	Identity   Identity
	Phase      string
	Parent     string
	Tool       string
	Backends   []string
	ObservedAt time.Time
	Bytes      []byte
	Envelope   []byte
}

//counterfeiter:generate . Producer
type Producer interface {
	Seal(Capsule, json.RawMessage) (emit.Result, error)
}
