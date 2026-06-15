package internal

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gomodule/redigo/redis"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

func (s *Server) executeCommand(auth *authResult, commandName string, args ...interface{}) (interface{}, int) {
	cmdUpper := strings.ToUpper(commandName)

	if err := s.isCommandAllowed(auth, cmdUpper); err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}
	if err := s.isDangerousCommandBlocked(cmdUpper); err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}

	if strings.ToLower(commandName) == "acl" && len(args) > 0 && strings.ToLower(fmt.Sprint(args[0])) == "resttoken" {
		return s.aclRestToken(args...)
	}

	if s.Metrics != nil {
		s.Metrics.Record(cmdUpper)
	}

	conn := s.RedisPool.Get()
	defer conn.Close()

	if err := conn.Err(); err != nil {
		s.Logger.Error("Redis connection error", zap.Error(err))
		return errorResult{Error: "ERR redis connection failed"}, fasthttp.StatusServiceUnavailable
	}

	if auth != nil {
		if err := s.authRedisConn(conn, auth.creds); err != nil {
			return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
		}
	}

	res, err := conn.Do(commandName, args...)
	if err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}
	return successResult{Result: s.convertRedisResult(res, false)}, fasthttp.StatusOK
}

func (s *Server) executeMultiExec(auth *authResult, requests [][]interface{}) (interface{}, int) {
	if len(requests) == 0 {
		return errorResult{Error: "ERR empty transaction request"}, fasthttp.StatusBadRequest
	}

	for _, req := range requests {
		if len(req) == 0 {
			return errorResult{Error: "ERR empty transaction command"}, fasthttp.StatusBadRequest
		}
		if err := s.isCommandAllowed(auth, strings.ToUpper(fmt.Sprint(req[0]))); err != nil {
			return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
		}
		if err := s.isDangerousCommandBlocked(strings.ToUpper(fmt.Sprint(req[0]))); err != nil {
			return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
		}
	}

	if s.Metrics != nil {
		s.Metrics.Record("MULTI-EXEC")
	}

	conn := s.RedisPool.Get()
	defer conn.Close()

	if err := conn.Err(); err != nil {
		return errorResult{Error: "ERR redis connection failed"}, fasthttp.StatusServiceUnavailable
	}
	if auth != nil {
		if err := s.authRedisConn(conn, auth.creds); err != nil {
			return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
		}
	}

	if err := conn.Send("MULTI"); err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}
	for _, req := range requests {
		cmdArgs := make([]interface{}, len(req))
		copy(cmdArgs, req)
		if err := conn.Send(fmt.Sprint(cmdArgs[0]), cmdArgs[1:]...); err != nil {
			return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
		}
	}

	execResult, err := conn.Do("EXEC")
	if err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}

	values, err := redis.Values(execResult, nil)
	if err != nil {
		return errorResult{Error: err.Error()}, fasthttp.StatusBadRequest
	}

	results := make([]interface{}, len(values))
	for i, val := range values {
		if errVal, ok := val.(error); ok {
			results[i] = errorResult{Error: errVal.Error()}
		} else {
			results[i] = successResult{Result: s.convertRedisResult(val, false)}
		}
	}
	return results, fasthttp.StatusOK
}

func decodeSegment(seg string) string {
	decoded, err := url.PathUnescape(seg)
	if err != nil {
		return seg
	}
	return decoded
}

func parsePathSegments(ctx *fasthttp.RequestCtx) []string {
	endpoint := string(ctx.Path())
	raw := strings.Split(strings.TrimPrefix(endpoint, "/"), "/")
	segments := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg != "" {
			segments = append(segments, decodeSegment(seg))
		}
	}

	if len(ctx.PostBody()) > 0 {
		segments = append(segments, string(ctx.PostBody()))
	}

	ctx.QueryArgs().VisitAll(func(key, value []byte) {
		if string(key) == "_token" {
			return
		}
		segments = append(segments, decodeSegment(string(key)))
		if len(value) > 0 {
			segments = append(segments, decodeSegment(string(value)))
		}
	})

	return segments
}
