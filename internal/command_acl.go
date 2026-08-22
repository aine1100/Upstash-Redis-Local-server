package internal

import (
	"strings"
	"sync"

	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"
)

// CommandACL caches which Redis commands are write/admin operations.
type CommandACL struct {
	mu     sync.RWMutex
	writes map[string]bool
}

func NewCommandACL() *CommandACL {
	return &CommandACL{writes: defaultWriteCommands()}
}

func defaultWriteCommands() map[string]bool {
	return map[string]bool{
		"SET": true, "SETEX": true, "SETNX": true, "MSET": true, "MSETNX": true,
		"DEL": true, "UNLINK": true, "INCR": true, "INCRBY": true, "DECR": true,
		"DECRBY": true, "APPEND": true, "GETSET": true, "HSET": true, "HMSET": true,
		"HINCRBY": true, "HINCRBYFLOAT": true, "HDEL": true, "LPUSH": true,
		"RPUSH": true, "LPOP": true, "RPOP": true, "LSET": true, "LREM": true,
		"LTRIM": true, "SADD": true, "SREM": true, "SPOP": true, "SMOVE": true,
		"ZADD": true, "ZREM": true, "ZINCRBY": true, "EXPIRE": true, "EXPIREAT": true,
		"PERSIST": true, "RENAME": true, "RENAMENX": true, "PUBLISH": true,
		"XADD": true, "XDEL": true, "XTRIM": true, "JSON.SET": true, "JSON.DEL": true,
		"MULTI": true, "EXEC": true, "DISCARD": true, "RESTORE": true, "MIGRATE": true,
	}
}

// LoadFromRedis introspects COMMAND flags and merges write/admin commands into the cache.
func (acl *CommandACL) LoadFromRedis(pool *redis.Pool, logger *zap.Logger) {
	conn := pool.Get()
	defer conn.Close()

	raw, err := redis.Values(conn.Do("COMMAND"))
	if err != nil {
		if logger != nil {
			logger.Warn("COMMAND introspection unavailable, using built-in write list", zap.Error(err))
		}
		return
	}

	writes := defaultWriteCommands()
	for _, entry := range raw {
		parts, ok := entry.([]interface{})
		if !ok || len(parts) < 3 {
			continue
		}
		name, _ := redis.String(parts[0], nil)
		flags, _ := redis.Strings(parts[2], nil)
		name = strings.ToUpper(name)
		for _, f := range flags {
			switch strings.ToLower(f) {
			case "write", "admin", "set", "insert", "delete", "dangerous":
				writes[name] = true
			}
		}
	}

	acl.mu.Lock()
	acl.writes = writes
	acl.mu.Unlock()
	if logger != nil {
		logger.Info("loaded command ACL from Redis COMMAND", zap.Int("write_commands", len(writes)))
	}
}

func (acl *CommandACL) IsWrite(command string) bool {
	if acl == nil {
		return defaultWriteCommands()[strings.ToUpper(command)]
	}
	acl.mu.RLock()
	defer acl.mu.RUnlock()
	return acl.writes[strings.ToUpper(command)]
}
