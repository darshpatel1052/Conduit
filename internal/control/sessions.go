package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const SessionCookie = "conduit_session"

type Session struct {
	AdminID  string    `json:"admin_id"`
	CSRF     string    `json:"csrf"`
	IssuedAt time.Time `json:"issued_at"`
}

type Sessions struct {
	redis *redis.Client
	ttl   time.Duration
}

func NewSessions(ctx context.Context, redisURL string, ttl time.Duration) (*Sessions, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse session Redis URL: %w", err)
	}
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping session Redis: %w", err)
	}
	return &Sessions{redis: client, ttl: ttl}, nil
}

func (s *Sessions) Close() error                   { return s.redis.Close() }
func (s *Sessions) Ping(ctx context.Context) error { return s.redis.Ping(ctx).Err() }

func (s *Sessions) Create(ctx context.Context, adminID string) (token string, session Session, err error) {
	token = randomToken(32)
	session = Session{AdminID: adminID, CSRF: randomToken(24), IssuedAt: time.Now().UTC()}
	payload, _ := json.Marshal(session)
	if err := s.redis.Set(ctx, sessionKey(token), payload, s.ttl).Err(); err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

func (s *Sessions) Get(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrInvalidLogin
	}
	payload, err := s.redis.Get(ctx, sessionKey(token)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrInvalidLogin
		}
		return nil, err
	}
	var session Session
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, ErrInvalidLogin
	}
	return &session, nil
}

func (s *Sessions) Revoke(ctx context.Context, token string) error {
	return s.redis.Del(ctx, sessionKey(token)).Err()
}

// AllowLogin limits each email and source address independently. A successful
// login clears both counters; failures do not reveal which identity existed.
func (s *Sessions) AllowLogin(ctx context.Context, email, sourceIP string) (bool, error) {
	keys := []string{"conduit:login:email:" + sha256Hex(normalizeEmail(email)), "conduit:login:ip:" + sha256Hex(sourceIP)}
	for _, key := range keys {
		count, err := s.redis.Incr(ctx, key).Result()
		if err != nil {
			return false, err
		}
		if count == 1 {
			_ = s.redis.Expire(ctx, key, 15*time.Minute).Err()
		}
		if count > 10 {
			return false, nil
		}
	}
	return true, nil
}

func (s *Sessions) ClearLoginAttempts(ctx context.Context, email, sourceIP string) {
	_ = s.redis.Del(ctx, "conduit:login:email:"+sha256Hex(normalizeEmail(email)), "conduit:login:ip:"+sha256Hex(sourceIP)).Err()
}

func sessionKey(token string) string { return "conduit:session:" + sha256Hex(token) }
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func randomToken(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
