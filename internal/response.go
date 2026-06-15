package internal

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

type errorResult struct {
	Error string `json:"error"`
}

type successResult struct {
	Result interface{} `json:"result"`
}

func (s *Server) wantsBase64(ctx *fasthttp.RequestCtx) bool {
	return string(ctx.Request.Header.Peek("Upstash-Encoding")) == "base64"
}

func (s *Server) wantsResp2(ctx *fasthttp.RequestCtx) bool {
	return string(ctx.Request.Header.Peek("Upstash-Response-Format")) == "resp2"
}

func (s *Server) convertRedisResult(res interface{}, useBase64 bool) interface{} {
	switch v := res.(type) {
	case []byte:
		if useBase64 {
			return base64.StdEncoding.EncodeToString(v)
		}
		if utf8.Valid(v) {
			return string(v)
		}
		return string(v)
	case []interface{}:
		for i, val := range v {
			v[i] = s.convertRedisResult(val, useBase64)
		}
		return v
	case error:
		return v.Error()
	default:
		return v
	}
}

func (s *Server) respond(ctx *fasthttp.RequestCtx, data interface{}, status int) {
	if s.wantsResp2(ctx) {
		s.respondResp2(ctx, data, status)
		return
	}

	ctx.SetContentType("application/json; charset=utf-8")
	ctx.SetStatusCode(status)
	if data == nil {
		return
	}

	useBase64 := s.wantsBase64(ctx)
	encoded := encodeResponseData(data, useBase64)

	b, err := json.Marshal(encoded)
	if err != nil {
		s.Logger.Error("failed to marshal response", zap.Error(err))
		s.writeJSON(ctx, errorResult{Error: fmt.Sprintf("something went wrong: %v", err)}, fasthttp.StatusInternalServerError)
		return
	}
	ctx.SetBody(b)
}

func encodeResponseData(data interface{}, useBase64 bool) interface{} {
	switch v := data.(type) {
	case successResult:
		return successResult{Result: convertValue(v.Result, useBase64)}
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = encodeResponseData(item, useBase64)
		}
		return out
	default:
		return data
	}
}

func convertValue(val interface{}, useBase64 bool) interface{} {
	switch v := val.(type) {
	case []byte:
		if useBase64 {
			if string(v) == "OK" {
				return "OK"
			}
			return base64.StdEncoding.EncodeToString(v)
		}
		return string(v)
	case string:
		if useBase64 && v != "OK" {
			return base64.StdEncoding.EncodeToString([]byte(v))
		}
		return v
	case []interface{}:
		for i, item := range v {
			v[i] = convertValue(item, useBase64)
		}
		return v
	default:
		return v
	}
}

func (s *Server) writeJSON(ctx *fasthttp.RequestCtx, data interface{}, status int) {
	ctx.SetContentType("application/json; charset=utf-8")
	ctx.SetStatusCode(status)
	b, _ := json.Marshal(data)
	ctx.SetBody(b)
}

func (s *Server) respondResp2(ctx *fasthttp.RequestCtx, data interface{}, status int) {
	if s.wantsBase64(ctx) {
		s.writeJSON(ctx, errorResult{Error: "ERR Upstash-Encoding base64 is not allowed with resp2 format"}, fasthttp.StatusBadRequest)
		return
	}

	ctx.SetContentType("application/octet-stream")
	ctx.SetStatusCode(status)
	if data == nil {
		return
	}

	switch v := data.(type) {
	case successResult:
		ctx.SetBodyString(formatResp2(v.Result))
	case errorResult:
		ctx.SetBodyString("-" + v.Error + "\r\n")
	default:
		b, _ := json.Marshal(data)
		ctx.SetBody(b)
	}
}

func formatResp2(val interface{}) string {
	switch v := val.(type) {
	case []byte:
		return fmt.Sprintf("+%s\r\n", string(v))
	case string:
		if v == "OK" {
			return "+OK\r\n"
		}
		return fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)
	case nil:
		return "$-1\r\n"
	case int64:
		return fmt.Sprintf(":%d\r\n", v)
	case int:
		return fmt.Sprintf(":%d\r\n", v)
	default:
		return fmt.Sprintf("+%v\r\n", v)
	}
}

func (s *Server) setCORS(ctx *fasthttp.RequestCtx) {
	origin := s.corsOrigin()
	ctx.Response.Header.Set("Access-Control-Allow-Origin", origin)
	if origin != "*" {
		ctx.Response.Header.Set("Vary", "Origin")
	}
	ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, HEAD, OPTIONS")
	ctx.Response.Header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Upstash-Encoding, Upstash-Response-Format, Accept")
	ctx.Response.Header.Set("Access-Control-Max-Age", "86400")
}
