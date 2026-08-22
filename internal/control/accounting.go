package control

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// Accounting holds the active monthly cost counter in Redis. Values are
// integer micro-dollars, avoiding floating-point drift across replicas.
type Accounting struct{ redis *redis.Client }

func NewAccounting(ctx context.Context, redisURL string) (*Accounting, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping accounting Redis: %w", err)
	}
	return &Accounting{redis: client}, nil
}
func (a *Accounting) Close() error                   { return a.redis.Close() }
func (a *Accounting) Ping(ctx context.Context) error { return a.redis.Ping(ctx).Err() }
func (a *Accounting) Current(ctx context.Context, projectID string) (float64, error) {
	value, err := a.redis.Get(ctx, budgetKey(projectID, time.Now())).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return float64(value) / 1_000_000, err
}
func (a *Accounting) Debit(ctx context.Context, projectID string, costUSD float64) error {
	if costUSD <= 0 {
		return nil
	}
	key := budgetKey(projectID, time.Now())
	pipe := a.redis.TxPipeline()
	pipe.IncrBy(ctx, key, int64(math.Round(costUSD*1_000_000)))
	pipe.Expire(ctx, key, 400*24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}
func budgetKey(projectID string, now time.Time) string {
	return fmt.Sprintf("conduit:budget:%s:%04d-%02d", projectID, now.UTC().Year(), now.UTC().Month())
}
