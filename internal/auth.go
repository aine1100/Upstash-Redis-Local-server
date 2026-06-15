package internal

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/gomodule/redigo/redis"
	"github.com/valyala/fasthttp"
)

type credentials struct {
	Username string
	Password string
}

type authResult struct {
	creds    credentials
	readOnly bool
}

var readOnlyBlocked = map[string]bool{
	"KEYS": true, "SCAN": true, "FLUSHALL": true, "FLUSHDB": true,
	"RANDOMKEY": true, "DEBUG": true, "CONFIG": true, "SHUTDOWN": true,
}

var writeCommands = map[string]bool{
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

func (s *Server) parseToken(ctx *fasthttp.RequestCtx) string {
	token := string(ctx.Request.Header.Peek("Authorization"))
	if token != "" {
		return strings.TrimPrefix(token, "Bearer ")
	}
	if s.disableQueryToken() {
		return ""
	}
	if queryToken := string(ctx.QueryArgs().Peek("_token")); queryToken != "" {
		return queryToken
	}
	return ""
}

func (s *Server) authenticate(ctx *fasthttp.RequestCtx) (*authResult, error) {
	token := s.parseToken(ctx)
	if token == "" {
		return nil, errors.New("invalid token")
	}
	if token == s.APIToken {
		return &authResult{creds: credentials{}}, nil
	}
	if s.ReadOnlyToken != "" && token == s.ReadOnlyToken {
		return &authResult{creds: credentials{}, readOnly: true}, nil
	}
	s.mutex.RLock()
	credential, found := s.credentials[token]
	s.mutex.RUnlock()
	if !found {
		return nil, errors.New("invalid token")
	}
	return &authResult{creds: credential}, nil
}

func (s *Server) isCommandAllowed(auth *authResult, commandName string) error {
	if auth == nil || !auth.readOnly {
		return nil
	}
	cmd := strings.ToUpper(commandName)
	if writeCommands[cmd] {
		return fmt.Errorf("NOPERM this user has no permissions to run the '%s' command", cmd)
	}
	if readOnlyBlocked[cmd] {
		return fmt.Errorf("NOPERM this user has no permissions to run the '%s' command", cmd)
	}
	return nil
}

func (s *Server) authRedisConn(conn redis.Conn, creds credentials) error {
	if creds.Username == "" && creds.Password == "" {
		return nil
	}
	if creds.Password == "" {
		_, err := conn.Do("AUTH", creds.Username)
		return err
	}
	_, err := conn.Do("AUTH", creds.Username, creds.Password)
	return err
}

func (s *Server) aclRestToken(args ...interface{}) (interface{}, int) {
	if len(args) != 3 {
		return errorResult{Error: "ERR invalid syntax. Usage: ACL RESTTOKEN username password"}, fasthttp.StatusBadRequest
	}
	user, pwd := fmt.Sprint(args[1]), fmt.Sprint(args[2])

	conn := s.RedisPool.Get()
	defer conn.Close()
	if err := s.authRedisConn(conn, credentials{Username: user, Password: pwd}); err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}

	var buf [48]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusInternalServerError
	}
	token := base64.URLEncoding.EncodeToString(buf[:])

	s.mutex.Lock()
	if s.credentials == nil {
		s.credentials = make(map[string]credentials)
	}
	s.credentials[token] = credentials{Username: user, Password: pwd}
	s.mutex.Unlock()

	return successResult{Result: token}, fasthttp.StatusOK
}
