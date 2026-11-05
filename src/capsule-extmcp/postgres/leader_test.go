package postgres_test

import (
	"cf-agentgateway-capsule/postgres"
	"context"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"time"
)

var _ = Describe("PostgreSQL leader election", func() {
	It("permits only one leader and transfers after session termination", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		first, err := postgres.TryLeadership(ctx, pool, "leader-test")
		Expect(err).NotTo(HaveOccurred())
		Expect(first).NotTo(BeNil())
		DeferCleanup(first.Close)
		second, err := postgres.TryLeadership(ctx, pool, "leader-test")
		Expect(err).NotTo(HaveOccurred())
		Expect(second).To(BeNil())
		Expect(first.Close()).To(Succeed())
		second, err = postgres.TryLeadership(ctx, pool, "leader-test")
		Expect(err).NotTo(HaveOccurred())
		Expect(second).NotTo(BeNil())
		DeferCleanup(second.Close)
	})
	It("detects a lost database session and permits a replacement leader", func() {
		ctx := context.Background()
		first, err := postgres.TryLeadership(ctx, pool, "leader-loss-test")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(first.Close)
		_, err = pool.Exec(ctx, "SELECT pg_terminate_backend($1)", first.BackendPID())
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error { return first.Heartbeat(ctx) }).Should(HaveOccurred())
		second, err := postgres.TryLeadership(ctx, pool, "leader-loss-test")
		Expect(err).NotTo(HaveOccurred())
		Expect(second).NotTo(BeNil())
		DeferCleanup(second.Close)
	})
})
