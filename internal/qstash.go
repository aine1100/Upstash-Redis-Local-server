package internal

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

const (
	qstashQueueKey  = "qstash:queue"
	qstashDLQKey    = "qstash:dlq"
	qstashMsgPrefix = "qstash:msg:"
	deliveredTTL    = 3600 // keep delivered/failed messages 1h for inspection
)

// QStash is a local emulator of Upstash QStash: an HTTP message queue that
// accepts a message destined for a URL, optionally delays it, and delivers it
// via HTTP POST with retries and a dead-letter queue. Backed by Redis so it
// survives restarts.
type QStash struct {
	pool       *redis.Pool
	logger     *zap.Logger
	httpClient *http.Client
	interval   time.Duration
	stop       chan struct{}
	signingKey string
}

type qstashMessage struct {
	ID                string            `json:"messageId"`
	Destination       string            `json:"destination"`
	Body              string            `json:"body"`
	Headers           map[string]string `json:"headers,omitempty"`
	Retries           int               `json:"retries"`
	MaxRetries        int               `json:"maxRetries"`
	State             string            `json:"state"`
	CreatedAt         int64             `json:"createdAt"`
	NotBefore         int64             `json:"notBefore"`
	LastError         string            `json:"lastError,omitempty"`
	ScheduleInterval  string            `json:"scheduleInterval,omitempty"`
}

// NewQStash builds a QStash worker bound to a Redis pool.
func NewQStash(pool *redis.Pool, logger *zap.Logger) *QStash {
	return &QStash{
		pool:       pool,
		logger:     logger,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		interval:   time.Second,
		stop:       make(chan struct{}),
	}
}

// SetSigningKey enables Upstash-Signature HMAC headers on delivered messages.
func (q *QStash) SetSigningKey(key string) {
	q.signingKey = key
}

// SetPollInterval overrides how often the queue is scanned for due messages.
func (q *QStash) SetPollInterval(d time.Duration) {
	if d > 0 {
		q.interval = d
	}
}

// Start launches the background delivery loop.
func (q *QStash) Start() {
	go q.loop()
}

// Stop halts the delivery loop.
func (q *QStash) Stop() {
	close(q.stop)
}

func (q *QStash) loop() {
	ticker := time.NewTicker(q.interval)
	defer ticker.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-ticker.C:
			q.processDue()
		}
	}
}

func (q *QStash) processDue() {
	conn := q.pool.Get()
	defer conn.Close()
	if conn.Err() != nil {
		return
	}

	now := time.Now().UnixMilli()
	ids, err := redis.Strings(conn.Do("ZRANGEBYSCORE", qstashQueueKey, 0, now))
	if err != nil {
		return
	}
	for _, id := range ids {
		// Claim the message so overlapping ticks can't double-deliver.
		if removed, _ := redis.Int(conn.Do("ZREM", qstashQueueKey, id)); removed == 0 {
			continue
		}
		q.deliver(conn, id)
	}
}

func (q *QStash) deliver(conn redis.Conn, id string) {
	msg, err := loadMessage(conn, id)
	if err != nil {
		return
	}

	req, err := http.NewRequest(http.MethodPost, msg.Destination, bytes.NewReader([]byte(msg.Body)))
	if err != nil {
		q.fail(conn, msg, "invalid destination: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Upstash-Message-Id", id)
	for k, v := range msg.Headers {
		req.Header.Set(k, v)
	}
	if sig := q.signBody(msg.Body); sig != "" {
		req.Header.Set("Upstash-Signature", sig)
	}

	resp, err := q.httpClient.Do(req)
	success := err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300
	if resp != nil {
		resp.Body.Close()
	}

	if success {
		msg.State = "delivered"
		msg.LastError = ""
		msg.Retries = 0
		saveMessage(conn, msg)
		if msg.ScheduleInterval != "" {
			if d := parseScheduleInterval(msg.ScheduleInterval); d > 0 {
				next := time.Now().Add(d).UnixMilli()
				msg.NotBefore = next
				msg.State = "scheduled"
				saveMessage(conn, msg)
				conn.Do("ZADD", qstashQueueKey, next, msg.ID)
				q.logger.Info("qstash rescheduled", zap.String("id", id), zap.Duration("every", d))
				return
			}
		}
		conn.Do("EXPIRE", qstashMsgPrefix+id, deliveredTTL)
		q.logger.Info("qstash delivered", zap.String("id", id), zap.String("dest", msg.Destination))
		return
	}

	reason := "delivery failed"
	if err != nil {
		reason = err.Error()
	} else if resp != nil {
		reason = fmt.Sprintf("destination returned status %d", resp.StatusCode)
	}
	q.retry(conn, msg, reason)
}

func (q *QStash) retry(conn redis.Conn, msg *qstashMessage, reason string) {
	msg.Retries++
	msg.LastError = reason
	if msg.Retries > msg.MaxRetries {
		q.fail(conn, msg, reason)
		return
	}
	// Exponential backoff capped at 60s.
	backoff := time.Duration(1<<uint(msg.Retries)) * time.Second
	if backoff > 60*time.Second {
		backoff = 60 * time.Second
	}
	next := time.Now().Add(backoff).UnixMilli()
	msg.NotBefore = next
	msg.State = "retrying"
	saveMessage(conn, msg)
	conn.Do("ZADD", qstashQueueKey, next, msg.ID)
	q.logger.Warn("qstash retry scheduled",
		zap.String("id", msg.ID),
		zap.Int("attempt", msg.Retries),
		zap.String("reason", reason),
	)
}

func (q *QStash) fail(conn redis.Conn, msg *qstashMessage, reason string) {
	msg.State = "failed"
	msg.LastError = reason
	saveMessage(conn, msg)
	conn.Do("LPUSH", qstashDLQKey, msg.ID)
	conn.Do("EXPIRE", qstashMsgPrefix+msg.ID, deliveredTTL)
	q.logger.Error("qstash message moved to DLQ", zap.String("id", msg.ID), zap.String("reason", reason))
}

func loadMessage(conn redis.Conn, id string) (*qstashMessage, error) {
	raw, err := redis.Bytes(conn.Do("GET", qstashMsgPrefix+id))
	if err != nil {
		return nil, err
	}
	var msg qstashMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func saveMessage(conn redis.Conn, msg *qstashMessage) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	conn.Do("SET", qstashMsgPrefix+msg.ID, b)
}

func newMessageID() string {
	var buf [16]byte
	rand.Read(buf[:])
	return "msg_" + hex.EncodeToString(buf[:])
}

// parseDelay accepts a Go duration ("10s", "2m") or a bare number of seconds.
func parseScheduleInterval(raw string) time.Duration {
	raw = strings.TrimSpace(strings.ToLower(raw))
	raw = strings.TrimPrefix(raw, "every ")
	if raw == "" {
		return 0
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return 0
}

func (q *QStash) signBody(body string) string {
	if q.signingKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(q.signingKey))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// parseDelay accepts a Go duration ("10s", "2m") or a bare number of seconds.
func parseDelay(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// --- HTTP handlers (wired in rest.go) ---

func (s *Server) handleQStashPublish(ctx *fasthttp.RequestCtx, rawDestination string) {
	destination := rawDestination
	if decoded, err := url.PathUnescape(rawDestination); err == nil {
		destination = decoded
	}
	if override := string(ctx.QueryArgs().Peek("url")); override != "" {
		destination = override
	}
	destination = strings.TrimSpace(destination)
	if destination == "" || !(strings.HasPrefix(destination, "http://") || strings.HasPrefix(destination, "https://")) {
		s.respond(ctx, errorResult{Error: "ERR destination must be an absolute http(s) URL"}, fasthttp.StatusBadRequest)
		return
	}

	delay := parseDelay(string(ctx.Request.Header.Peek("Upstash-Delay")))
	maxRetries := 3
	if r := string(ctx.Request.Header.Peek("Upstash-Retries")); r != "" {
		if n, err := strconv.Atoi(r); err == nil && n >= 0 {
			maxRetries = n
		}
	}

	// Forward any Upstash-Forward-* headers to the destination.
	forwarded := map[string]string{}
	ctx.Request.Header.VisitAll(func(key, value []byte) {
		k := string(key)
		if strings.HasPrefix(strings.ToLower(k), "upstash-forward-") {
			forwarded[k[len("upstash-forward-"):]] = string(value)
		}
	})

	now := time.Now()
	notBefore := now.Add(delay).UnixMilli()
	schedule := strings.TrimSpace(string(ctx.Request.Header.Peek("Upstash-Schedule")))
	msg := &qstashMessage{
		ID:               newMessageID(),
		Destination:      destination,
		Body:             string(ctx.PostBody()),
		Headers:          forwarded,
		MaxRetries:       maxRetries,
		State:            "queued",
		CreatedAt:        now.UnixMilli(),
		NotBefore:        notBefore,
		ScheduleInterval: schedule,
	}

	conn := s.RedisPool.Get()
	defer conn.Close()
	saveMessage(conn, msg)
	if _, err := conn.Do("ZADD", qstashQueueKey, notBefore, msg.ID); err != nil {
		s.respond(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}

	s.writeJSON(ctx, map[string]string{"messageId": msg.ID}, fasthttp.StatusOK)
}

func (s *Server) handleQStashList(ctx *fasthttp.RequestCtx) {
	conn := s.RedisPool.Get()
	defer conn.Close()

	keys, err := scanKeys(conn, qstashMsgPrefix+"*", 0)
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}

	messages := make([]qstashMessage, 0, len(keys))
	for _, key := range keys {
		id := strings.TrimPrefix(key, qstashMsgPrefix)
		if msg, err := loadMessage(conn, id); err == nil {
			messages = append(messages, *msg)
		}
	}
	s.writeJSON(ctx, map[string]interface{}{"messages": messages, "count": len(messages)}, fasthttp.StatusOK)
}

func (s *Server) handleQStashDLQ(ctx *fasthttp.RequestCtx) {
	conn := s.RedisPool.Get()
	defer conn.Close()

	ids, err := redis.Strings(conn.Do("LRANGE", qstashDLQKey, 0, -1))
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}
	messages := make([]qstashMessage, 0, len(ids))
	for _, id := range ids {
		if msg, err := loadMessage(conn, id); err == nil {
			messages = append(messages, *msg)
		}
	}
	s.writeJSON(ctx, map[string]interface{}{"messages": messages, "count": len(messages)}, fasthttp.StatusOK)
}

func (s *Server) handleQStashGetMessage(ctx *fasthttp.RequestCtx, id string) {
	conn := s.RedisPool.Get()
	defer conn.Close()
	msg, err := loadMessage(conn, id)
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: "message not found"}, fasthttp.StatusNotFound)
		return
	}
	s.writeJSON(ctx, msg, fasthttp.StatusOK)
}

func (s *Server) handleQStashReplayDLQ(ctx *fasthttp.RequestCtx, id string) {
	conn := s.RedisPool.Get()
	defer conn.Close()

	msg, err := loadMessage(conn, id)
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: "message not found"}, fasthttp.StatusNotFound)
		return
	}
	conn.Do("LREM", qstashDLQKey, 0, id)
	msg.Retries = 0
	msg.LastError = ""
	msg.State = "queued"
	next := time.Now().UnixMilli()
	msg.NotBefore = next
	saveMessage(conn, msg)
	conn.Do("ZADD", qstashQueueKey, next, id)
	s.writeJSON(ctx, map[string]string{"messageId": id, "state": "queued"}, fasthttp.StatusOK)
}
