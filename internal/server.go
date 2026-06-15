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
}
