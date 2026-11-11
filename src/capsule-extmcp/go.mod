module cf-agentgateway-capsule

go 1.27.0

require (
	cf-mcp-config v0.0.0
	github.com/action-state-group/capsule-emit/go v0.3.0
	github.com/action-state-group/checkpointed-local-log/go v0.3.0
	github.com/agentgateway/agentgateway/api v0.0.0
	github.com/fxamacker/cbor/v2 v2.9.1
	github.com/jackc/pgx/v5 v5.8.0
	github.com/onsi/ginkgo/v2 v2.33.1
	github.com/onsi/gomega v1.44.0
	github.com/veraison/go-cose v1.3.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/Masterminds/semver/v3 v3.5.0 // indirect
	github.com/action-state-group/agent-action-capsule/go v0.7.0 // indirect
	github.com/datatrails/go-datatrails-merklelog/mmr v0.4.2-0.20250428132117-275103a34a08 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20260402051712-545e8a4df936 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/maxbrunsfeld/counterfeiter/v6 v6.14.0 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240409071808-615f978279ca // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260720211330-0afa2a65878a // indirect
	istio.io/istio v0.0.0-20261012224918-737d694fc48b // indirect
)

replace github.com/agentgateway/agentgateway/api => ../agentgateway/api

replace cf-mcp-config => ../mcpconfig

tool github.com/maxbrunsfeld/counterfeiter/v6
