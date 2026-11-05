package postgres

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// TestPostgresInventoryLatency measures inventory reads at the M2 record count.
// It is opt-in: set LINEA_FLEET_TEST_INVENTORY_RECORDS (for example 20000) in
// addition to the disposable fixture URL from scripts/fleet/test-postgres.ps1.
func TestPostgresInventoryLatency(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	records, _ := strconv.Atoi(os.Getenv("LINEA_FLEET_TEST_INVENTORY_RECORDS"))
	if base == "" || records <= 0 {
		t.Skip("requires isolated PostgreSQL fixture and LINEA_FLEET_TEST_INVENTORY_RECORDS")
	}
	const runs = 50
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_latency"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_latency?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, url, "latency-fleet", "bootstrap-admin"); err != nil {
		t.Fatal(err)
	}
	// Synthetic placement: 200 services, 50 namespaces, 1,000 hosts, 20 teams,
	// 10% unhealthy and ten agents with a rare key.
	seed, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := seed.Exec(ctx, `INSERT INTO fleet.agents (id,generation,connected_at,disconnected_at,first_seen_at,capabilities,healthy,health_observed_at,health_status,attributes)
		SELECT lpad(to_hex(n),32,'0'),1,now(),now(),now(),2053,n%10<>0,now(),'StatusOK',
			jsonb_build_object('service.name','svc-'||n%200,'service.version','1.'||n%3,'host.name','host-'||n%1000,
				'k8s.namespace.name','ns-'||n%50,'k8s.pod.name','pod-'||n,'team','team-'||n%20,'os.type','linux')
			|| CASE WHEN n%(GREATEST($1/10,1))=0 THEN '{"rare":"yes"}'::jsonb ELSE '{}'::jsonb END
		FROM generate_series(1,$1::int) n`, records); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, "ANALYZE fleet.agents"); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close(ctx)
	t.Logf("seeded %d agents in %s", records, time.Since(started).Round(time.Millisecond))
	s, err := Open(ctx, url, Options{ServerID: "latency-fleet", MaxAgents: records + 1, MaxEvents: 1000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	selector := func(text string) fleet.Selector {
		sel, err := fleet.ParseSelector(text)
		if err != nil {
			t.Fatal(err)
		}
		return sel
	}
	unhealthy := false
	middle := fleet.EncodeAfter("host-500", strings.Repeat("0", 32))
	cases := []struct {
		name  string
		query fleet.AgentQuery
	}{
		{"first page", fleet.AgentQuery{}},
		{"promoted =", fleet.AgentQuery{Selector: selector("service.name=svc-7")}},
		{"attribute =", fleet.AgentQuery{Selector: selector("team=team-3")}},
		{"promoted in", fleet.AgentQuery{Selector: selector("k8s.namespace.name in (ns-1,ns-2,ns-3)")}},
		{"rare exists", fleet.AgentQuery{Selector: selector("rare")}},
		{"no match", fleet.AgentQuery{Selector: selector("team=none")}},
		{"combined", fleet.AgentQuery{Selector: selector("k8s.namespace.name=ns-4,team=team-4"), Healthy: &unhealthy}},
		{"sorted middle", fleet.AgentQuery{Sort: "host.name", After: middle}},
		{"sorted selector", fleet.AgentQuery{Sort: "service.name", Selector: selector("team in (team-1,team-2)")}},
	}
	measure := func(name string, read func() error) {
		t.Helper()
		durations := make([]time.Duration, 0, runs)
		for range runs {
			start := time.Now()
			if err := read(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			durations = append(durations, time.Since(start))
		}
		slices.Sort(durations)
		p50, p99 := durations[runs/2], durations[runs*99/100]
		t.Logf("%-16s p50 %6.1f ms  p99 %6.1f ms", name, float64(p50.Microseconds())/1000, float64(p99.Microseconds())/1000)
		if p99 > 500*time.Millisecond {
			t.Errorf("%s p99 %s exceeds 500 ms", name, p99)
		}
	}
	for _, c := range cases {
		c.query.Limit = 100
		measure(c.name, func() error { _, err := s.ListAgents(ctx, c.query); return err })
	}
	measure("summary", func() error { _, err := s.Summary(ctx, ""); return err })
	measure("summary by svc", func() error { _, err := s.Summary(ctx, "service.name"); return err })
}
