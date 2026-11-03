package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync"
	"time"
)

// Leadership owns a session-scoped advisory lock on a dedicated connection.
// Close destroys the session rather than returning a potentially locked session
// to the pool. The workers must use a context canceled on Heartbeat failure.
type Leadership struct {
	conn *pgxpool.Conn
	once sync.Once
}

func TryLeadership(ctx context.Context, pool *pgxpool.Pool, logID string) (*Leadership, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte("capsule-checkpoint-leader/" + logID))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	var acquired bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, err
	}
	if !acquired {
		conn.Release()
		return nil, nil
	}
	return &Leadership{conn: conn}, nil
}
func (l *Leadership) Heartbeat(ctx context.Context) error { return l.conn.Conn().Ping(ctx) }
func (l *Leadership) BackendPID() uint32                  { return l.conn.Conn().PgConn().PID() }
func (l *Leadership) Close() error {
	var err error
	l.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = l.conn.Conn().Close(ctx)
		l.conn.Release()
	})
	return err
}
