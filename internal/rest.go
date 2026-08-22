package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

// Serve starts the HTTP server and blocks until a shutdown signal is received.
func (s *Server) Serve() {
	srv := &fasthttp.Server{
		Handler:               s.requestHandler,
		StreamRequestBody:     true,
		NoDefaultServerHeader: true,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		s.Logger.Info("shutdown signal received, draining connections", zap.String("signal", sig.String()))
		if err := srv.Shutdown(); err != nil {
			s.Logger.Error("graceful shutdown failed", zap.Error(err))
		}
	}()

	s.Logger.Info("upstash-redis-local booting on", zap.String("addr", s.Address))
	if err := srv.ListenAndServe(s.Address); err != nil {
		s.Logger.Fatal("Error in serving", zap.Error(err))
	}
	s.Logger.Info("server stopped cleanly")
}

func (s *Server) requestHandler(ctx *fasthttp.RequestCtx) {
	if s.LogRequests {
		start := time.Now()
		defer func() {
			s.Logger.Info("request",
				zap.ByteString("method", ctx.Method()),
				zap.ByteString("path", ctx.Path()),
				zap.Int("status", ctx.Response.StatusCode()),
				zap.Duration("took", time.Since(start)),
			)
		}()
	}

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
	if path == "/dashboard/api/monitor" {
		if err := s.authenticateDashboard(ctx); err != nil {
			s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
			return
		}
		s.handleDashboardMonitor(ctx)
		return
	}
	if path == "/dashboard/api/execute" && ctx.IsPost() {
		if err := s.authenticateDashboard(ctx); err != nil {
			s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
			return
		}
		s.handleDashboardExecute(ctx)
		return
	}
	if path == "/dashboard/api/snapshots" {
		if err := s.authenticateDashboard(ctx); err != nil {
			s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
			return
		}
		if ctx.IsGet() {
			s.handleSnapshotList(ctx)
		}
		return
	}
	if strings.HasPrefix(path, "/dashboard/api/snapshots/") {
		if err := s.authenticateDashboard(ctx); err != nil {
			s.respond(ctx, errorResult{Error: "Unauthorised"}, fasthttp.StatusUnauthorized)
			return
		}
		name := strings.Trim(strings.TrimPrefix(path, "/dashboard/api/snapshots/"), "/")
		if strings.HasSuffix(name, "/restore") && ctx.IsPost() {
			s.handleSnapshotRestore(ctx, strings.TrimSuffix(name, "/restore"))
			return
		}
		switch {
		case ctx.IsPost():
			s.handleSnapshotSave(ctx, name)
		case ctx.IsDelete():
			s.handleSnapshotDelete(ctx, name)
		default:
			s.respond(ctx, nil, fasthttp.StatusMethodNotAllowed)
		}
		return
	}

	if !ctx.IsGet() && !ctx.IsPost() && !ctx.IsHead() && !ctx.IsPut() && !ctx.IsDelete() {
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

	if s.Chaos != nil {
		s.Chaos.applyLatency()
		if msg, hit := s.Chaos.maybeError(); hit {
			s.respond(ctx, errorResult{Error: msg}, fasthttp.StatusInternalServerError)
			return
		}
	}

	if s.QStash != nil {
		original := string(ctx.URI().PathOriginal())
		method := string(ctx.Method())
		switch {
		case strings.HasPrefix(original, "/v2/publish/"):
			s.handleQStashPublish(ctx, strings.TrimPrefix(original, "/v2/publish/"))
			return
		case path == "/v2/messages":
			s.handleQStashList(ctx)
			return
		case strings.HasPrefix(path, "/v2/messages/"):
			s.handleQStashGetMessage(ctx, strings.TrimPrefix(path, "/v2/messages/"))
			return
		case path == "/v2/dlq":
			s.handleQStashDLQ(ctx)
			return
		case strings.HasPrefix(path, "/v2/dlq/") && strings.HasSuffix(path, "/replay") && method == "POST":
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/dlq/"), "/replay")
			s.handleQStashReplayDLQ(ctx, id)
			return
		}
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
