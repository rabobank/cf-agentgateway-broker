package postgres_test

import (
	"cf-agentgateway-capsule/postgres"
	"context"
	"fmt"
	"github.com/action-state-group/checkpointed-local-log/go/cll/conformance"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"os"
	"os/exec"
	"testing"
	"time"
)

// The upstream contract intentionally exposes a testing.T runner. Its native
// subtests supplement the Ginkgo behavioral specs.
func TestBackendConformance(t *testing.T) {
	// This native test is invoked separately after the Ginkgo suite provisions
	// its database only when CAPSULE_CONFORMANCE_DATABASE_URL is supplied.
	// The Ginkgo suite runs it through a subprocess below.
	url := os.Getenv("CAPSULE_CONFORMANCE_DATABASE_URL")
	if url == "" {
		return
	}
	conformance.Run(t, conformance.Config{Open: func(t *testing.T) any { return newConformanceStore(t, url) }, Reopen: func(t *testing.T, current any) any {
		s := current.(*postgres.Store)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		p, err := pgxpool.New(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		next, err := postgres.Open(context.Background(), p, s.LogID(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}})
}

var _ = Describe("Upstream backend contract", func() {
	It("is exercised against the same PostgreSQL backend", func() {
		// Native runner is executed by a child test process with its own handles.
		out, err := conformanceCommand(uri)
		Expect(err).NotTo(HaveOccurred(), "%s", out)
	})
})

func newConformanceStore(t *testing.T, url string) *postgres.Store {
	t.Helper()
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	s, err := postgres.Open(context.Background(), p, fmt.Sprintf("contract-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func conformanceCommand(url string) ([]byte, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackendConformance$", "-test.v")
	cmd.Env = append(os.Environ(), "CAPSULE_CONFORMANCE_DATABASE_URL="+url)
	return cmd.CombinedOutput()
}
