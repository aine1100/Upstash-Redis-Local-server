package internal

import (
	"sync"

	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"
)

// Server holds REST proxy configuration and runtime state.
type Server struct {
	Address       string
	APIToken      string
	ReadOnlyToken string
	RedisPool     *redis.Pool
	credentials   map[string]credentials
	mutex         sync.RWMutex
	Logger        *zap.Logger
	Metrics       *Metrics
	RateLimiter   *RateLimiter
	Security      SecurityConfig
	Chaos         *ChaosConfig
	LogRequests   bool
	Recorder      *Recorder
	QStash        *QStash
	CommandLog    *CommandLog
	CommandACL    *CommandACL
	SigningKey    string

	// Dial creates a standalone connection for long-lived streams (pub/sub,
	// monitor) so they never exhaust the shared request pool. Falls back to
	// the pool when nil.
	Dial func() (redis.Conn, error)
}

// streamConn returns a dedicated connection for long-lived SSE streams.
func (s *Server) streamConn() (redis.Conn, error) {
	if s.Dial != nil {
		return s.Dial()
	}
	conn := s.RedisPool.Get()
	return conn, conn.Err()
}
