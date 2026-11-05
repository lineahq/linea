package postgres

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

type queryStartKey struct{}

// Query text, arguments, command tags and driver errors are deliberately ignored.
func (s *Store) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStartKey{}, time.Now())
}
func (s *Store) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if start, ok := ctx.Value(queryStartKey{}).(time.Time); ok {
		s.queryDuration.Observe(time.Since(start))
	}
	if data.Err != nil {
		s.queryErrors.Add(1)
	}
}

func boolGauge(v bool) int {
	if v {
		return 1
	}
	return 0
}

// WriteMetrics snapshots in-memory driver and replica statistics; it never
// queries storage.
func (s *Store) WriteMetrics(w io.Writer) {
	stat := s.pool.Stat()
	fmt.Fprintf(w, "# TYPE linea_fleet_database_connections gauge\nlinea_fleet_database_connections{state=\"acquired\"} %d\nlinea_fleet_database_connections{state=\"idle\"} %d\n# TYPE linea_fleet_database_connection_limit gauge\nlinea_fleet_database_connection_limit %d\n# TYPE linea_fleet_database_pool_wait_seconds_total counter\nlinea_fleet_database_pool_wait_seconds_total %g\n# TYPE linea_fleet_database_pool_waits_total counter\nlinea_fleet_database_pool_waits_total %d\n# TYPE linea_fleet_database_pool_canceled_total counter\nlinea_fleet_database_pool_canceled_total %d\n", stat.AcquiredConns(), stat.IdleConns(), stat.MaxConns(), stat.EmptyAcquireWaitTime().Seconds(), stat.EmptyAcquireCount(), stat.CanceledAcquireCount())
	fmt.Fprintf(w, "# TYPE linea_fleet_database_query_errors_total counter\nlinea_fleet_database_query_errors_total %d\n# TYPE linea_fleet_database_write_errors_total counter\nlinea_fleet_database_write_errors_total %d\n# TYPE linea_fleet_database_flush_errors_total counter\nlinea_fleet_database_flush_errors_total %d\n", s.queryErrors.Load(), s.writeErrors.Load(), s.flushErrors.Load())
	s.writeReplicaMetrics(w)
	s.queryDuration.Snapshot().Write(w, "linea_fleet_database_query_duration_seconds")
	s.writeDuration.Snapshot().Write(w, "linea_fleet_database_write_duration_seconds")
}

func (s *Store) writeReplicaMetrics(w io.Writer) {
	queued := 0
	if s.writer != nil {
		queued = len(s.writer.queue)
	}
	age := time.Since(time.Unix(0, s.heartbeat.Load())).Seconds()
	fmt.Fprintf(w, "# TYPE linea_fleet_writer_queue gauge\nlinea_fleet_writer_queue %d\n# TYPE linea_fleet_writer_queue_limit gauge\nlinea_fleet_writer_queue_limit %d\n# TYPE linea_fleet_writer_batches_total counter\nlinea_fleet_writer_batches_total %d\n# TYPE linea_fleet_writer_batch_items_total counter\nlinea_fleet_writer_batch_items_total %d\n", queued, queueSize, s.batches.Load(), s.batchItems.Load())
	fmt.Fprintf(w, "# TYPE linea_fleet_replica_leader gauge\nlinea_fleet_replica_leader %d\n# TYPE linea_fleet_replica_heartbeat_age_seconds gauge\nlinea_fleet_replica_heartbeat_age_seconds %g\n# TYPE linea_fleet_agent_takeovers_total counter\nlinea_fleet_agent_takeovers_total %d\n# TYPE linea_fleet_agents_reaped_total counter\nlinea_fleet_agents_reaped_total %d\n# TYPE linea_fleet_agents_expired_total counter\nlinea_fleet_agents_expired_total %d\n", boolGauge(s.leader.Load()), age, s.takeovers.Load(), s.reaped.Load(), s.expired.Load())
	fmt.Fprintf(w, "# TYPE linea_fleet_rollout_controller_errors_total counter\nlinea_fleet_rollout_controller_errors_total %d\n", s.rolloutErrors.Load())
}
