package internal

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

// Serve starts the HTTP server.
func (s *Server) Serve() {
	s.Logger.Info("upstash-redis-local booting on", zap.String("addr", s.Address))
	if err := fasthttp.ListenAndServe(s.Address, s.requestHandler); err != nil {
		s.Logger.Fatal("Error in serving", zap.Error(err))
	}
}

func (s *Server) requestHandler(ctx *fasthttp.RequestCtx) {
	s.setCORS(ctx)
	if string(ctx.Method()) == "OPTIONS" {
		ctx.SetStatusCode(fasthttp.StatusNoContent)
		return
	}

	path := string(ctx.Path())

	if path == "/health" || path == "/ready" {
		s.handleHealth(ctx)
		return
	}

	if path == "/dashboard" || path == "/dashboard/" {
		s.handleDashboard(ctx)
		return
	}
	if path == "/dashboard/api/stats" {
		if err := s.authenticateDashboard(ctx); err != nil {
			s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
			return
		}
		s.handleDashboardStats(ctx)
		return
	}
	if path == "/dashboard/api/keys" {
		if err := s.authenticateDashboard(ctx); err != nil {
			s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
			return
		}
		s.handleDashboardKeys(ctx)
		return
	}

	if !ctx.IsGet() && !ctx.IsPost() && !ctx.IsHead() && !ctx.IsPut() {
		s.respond(ctx, nil, fasthttp.StatusMethodNotAllowed)
		return
	}

	if s.RateLimiter != nil {
		if ok, msg := s.RateLimiter.Allow(); !ok {
			s.respond(ctx, errorResult{Error: msg}, fasthttp.StatusTooManyRequests)
			return
		}
	}

	auth, err := s.authenticate(ctx)
	if err != nil {
		s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
		return
	}

	switch {
	case path == "" || path == "/":
		s.handleSingleExecute(ctx, auth)
		return
	case path == "/pipeline":
		s.handlePipelineExecute(ctx, auth)
		return
	case path == "/multi-exec":
		s.handleMultiExec(ctx, auth)
		return
	case path == "/monitor":
		s.handleMonitorSSE(ctx, auth)
		return
	case strings.HasPrefix(path, "/publish/"):
		segments := parsePathSegments(ctx)
		s.handlePublish(ctx, auth, segments)
		return
	case strings.HasPrefix(path, "/subscribe/"):
		channel := strings.TrimPrefix(path, "/subscribe/")
		channel = decodeSegment(strings.Split(channel, "/")[0])
		s.handleSubscribeSSE(ctx, auth, channel)
		return
	default:
		segments := parsePathSegments(ctx)
		if len(segments) == 0 {
			s.respond(ctx, errorResult{Error: "ERR empty command"}, fasthttp.StatusBadRequest)
			return
		}
		args := make([]interface{}, len(segments)-1)
		for i, data := range segments[1:] {
			args[i] = data
		}
		res, code := s.executeCommand(auth, segments[0], args...)
		s.respond(ctx, res, code)
	}
}

func (s *Server) handleSingleExecute(ctx *fasthttp.RequestCtx, auth *authResult) {
	var args []interface{}
	if err := json.Unmarshal(ctx.PostBody(), &args); err != nil {
		s.respond(ctx, errorResult{Error: "ERR failed to parse command"}, fasthttp.StatusBadRequest)
		return
	}
	if len(args) == 0 {
		s.respond(ctx, errorResult{Error: "ERR empty command"}, fasthttp.StatusBadRequest)
		return
	}
	result, code := s.executeCommand(auth, fmt.Sprint(args[0]), args[1:]...)
	s.respond(ctx, result, code)
}

func (s *Server) handlePipelineExecute(ctx *fasthttp.RequestCtx, auth *authResult) {
	var pipelineRequests [][]interface{}
	if err := json.Unmarshal(ctx.PostBody(), &pipelineRequests); err != nil {
		s.respond(ctx, errorResult{Error: "ERR failed to parse pipeline request"}, fasthttp.StatusBadRequest)
		return
	}
	if len(pipelineRequests) == 0 {
		s.respond(ctx, errorResult{Error: "ERR empty pipeline request"}, fasthttp.StatusBadRequest)
		return
	}

	var results []interface{}
	for _, request := range pipelineRequests {
		if len(request) == 0 {
			results = append(results, errorResult{Error: "ERR empty pipeline command"})
			continue
		}
		result, _ := s.executeCommand(auth, fmt.Sprint(request[0]), request[1:]...)
		results = append(results, result)
	}
	s.respond(ctx, results, fasthttp.StatusOK)
}

func (s *Server) handleMultiExec(ctx *fasthttp.RequestCtx, auth *authResult) {
	var requests [][]interface{}
	if err := json.Unmarshal(ctx.PostBody(), &requests); err != nil {
		s.respond(ctx, errorResult{Error: "ERR failed to parse transaction request"}, fasthttp.StatusBadRequest)
		return
	}
	result, code := s.executeMultiExec(auth, requests)
	s.respond(ctx, result, code)
}
