package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"upstash-redis-local/internal"
)

var Version = "development"

type Cmd struct {
	RedisAddr              string
	Addr                   string
	ApiToken               string
	ReadOnlyToken          string
	MaxRetries             int
	RetryDelayMs           int
	SimulateQuota          int64
	SimulateRPS            int
	LocalhostOnly          bool
	DisableQueryToken      bool
	CORSOrigin             string
	RequireDashboardAuth   bool
	BlockDangerousCommands bool
	SecureMode             bool
}

func (c *Cmd) Validate() error {
	if c.ApiToken == "" {
		return errors.New("API Token empty")
	}
	if c.RedisAddr == "" {
		return errors.New("redis Addr empty")
	}
	if c.Addr == "" {
		return errors.New("webserver addr empty")
	}
	return nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		switch strings.ToLower(value) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return defaultValue
}

func main() {
	setupFlags(flag.CommandLine)

	defaultRedis := getEnvOrDefault("REDIS_ADDR", ":6379")
	defaultAddr := getEnvOrDefault("UPSTASH_ADDR", ":8000")
	defaultToken := getEnvOrDefault("UPSTASH_TOKEN", "upstash")
	defaultReadOnly := getEnvOrDefault("UPSTASH_READONLY_TOKEN", "")

	redisAddr := flag.String("redis", defaultRedis, "Redis server address (env: REDIS_ADDR)")
	addr := flag.String("addr", defaultAddr, "Webserver address (env: UPSTASH_ADDR)")
	apiToken := flag.String("token", defaultToken, "API token (env: UPSTASH_TOKEN)")
	readOnlyToken := flag.String("readonly-token", defaultReadOnly, "Read-only API token (env: UPSTASH_READONLY_TOKEN)")
	maxRetries := flag.Int("max-retries", 10, "Max connection retries on startup")
	retryDelay := flag.Int("retry-delay", 1000, "Delay between retries in milliseconds")
	simulateQuota := flag.Int64("simulate-quota", getEnvInt64("SIMULATE_QUOTA", 0), "Simulate Upstash daily quota (0=unlimited)")
	simulateRPS := flag.Int("simulate-rps", getEnvInt("SIMULATE_RPS", 0), "Simulate Upstash RPS limit (0=unlimited)")
	localhostOnly := flag.Bool("localhost-only", getEnvBool("UPSTASH_LOCALHOST_ONLY", false), "Bind to 127.0.0.1 only")
	disableQueryToken := flag.Bool("disable-query-token", getEnvBool("UPSTASH_DISABLE_QUERY_TOKEN", false), "Reject ?_token= query auth")
	corsOrigin := flag.String("cors-origin", getEnvOrDefault("UPSTASH_CORS_ORIGIN", "*"), "CORS Allow-Origin (* = any)")
	requireDashboardAuth := flag.Bool("require-dashboard-auth", getEnvBool("UPSTASH_REQUIRE_DASHBOARD_AUTH", true), "Require auth for dashboard API")
	blockDangerous := flag.Bool("block-dangerous-commands", getEnvBool("UPSTASH_BLOCK_DANGEROUS_COMMANDS", false), "Block FLUSHALL, CONFIG, SHUTDOWN, etc.")
	secureMode := flag.Bool("secure", getEnvBool("UPSTASH_SECURE", false), "Enable all security hardening flags")
	help := flag.Bool("help", false, "Print help message")
	flag.Parse()

	listenAddr := internal.ResolveListenAddr(*addr, *localhostOnly)
	if *secureMode {
		*localhostOnly = true
		*disableQueryToken = true
		*requireDashboardAuth = true
		*blockDangerous = true
		listenAddr = internal.ResolveListenAddr(*addr, true)
	}

	cmd := Cmd{
		RedisAddr:              *redisAddr,
		ApiToken:               *apiToken,
		ReadOnlyToken:          *readOnlyToken,
		Addr:                   listenAddr,
		MaxRetries:             *maxRetries,
		RetryDelayMs:           *retryDelay,
		SimulateQuota:          *simulateQuota,
		SimulateRPS:            *simulateRPS,
		LocalhostOnly:          *localhostOnly,
		DisableQueryToken:      *disableQueryToken,
		CORSOrigin:             *corsOrigin,
		RequireDashboardAuth:   *requireDashboardAuth,
		BlockDangerousCommands: *blockDangerous,
		SecureMode:             *secureMode,
	}

	if *help {
		printHelp()
		return
	}

	if err := cmd.Validate(); err != nil {
		log.Fatalf("validation error: %v", err)
	}

	config := zap.NewProductionConfig()
	config.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout(time.RFC3339)

	logger, err := config.Build()
	if err != nil {
		log.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Sync()

	pool := createRedisPool(cmd.RedisAddr, cmd.MaxRetries, cmd.RetryDelayMs, logger)
	defer pool.Close()

	if err := testRedisConnection(pool, logger); err != nil {
		log.Fatalf("failed to connect to Redis: %v", err)
	}

	var limiter *internal.RateLimiter
	if cmd.SimulateQuota > 0 || cmd.SimulateRPS > 0 {
		limiter = internal.NewRateLimiter(cmd.SimulateQuota, cmd.SimulateRPS)
		logger.Info("rate limit simulation enabled",
			zap.Int64("quota", cmd.SimulateQuota),
			zap.Int("rps", cmd.SimulateRPS),
		)
	}

	sec := internal.SecurityConfig{
		DisableQueryToken:      cmd.DisableQueryToken,
		CORSOrigin:             cmd.CORSOrigin,
		RequireDashboardAuth:   cmd.RequireDashboardAuth,
		BlockDangerousCommands: cmd.BlockDangerousCommands,
	}
	internal.LogSecurityWarnings(logger, cmd.Addr, cmd.ApiToken, cmd.ReadOnlyToken, sec, cmd.SecureMode)

	server := internal.Server{
		Address:       cmd.Addr,
		APIToken:      cmd.ApiToken,
		ReadOnlyToken: cmd.ReadOnlyToken,
		RedisPool:     pool,
		Logger:        logger,
		Metrics:       internal.NewMetrics(),
		RateLimiter:   limiter,
		Security:      sec,
	}
	server.Serve()
}

func setupFlags(f *flag.FlagSet) {
	f.Usage = func() {
		printHelp()
	}
}

func printHelp() {
	fmt.Printf(`
upstash-redis-local %s
A local server that mimics upstash-redis for local testing purposes!

       * Unlimited local requests — no cloud rate limits
       * REST API compatible with @upstash/redis SDK
       * Dashboard at /dashboard

USAGE:
	upstash-redis-local
	upstash-redis-local --token local-dev-token --addr :8000 --redis :6379

ARGUMENTS:
	--token            TOKEN  Full-access API token (default: upstash)
	--readonly-token   TOKEN  Read-only API token (optional)
	--addr             ADDR   Listen address (default: :8000)
	--redis            ADDR   Redis address (default: :6379)
	--max-retries      N      Startup connection retries (default: 10)
	--retry-delay      MS     Retry delay in ms (default: 1000)
	--simulate-quota   N      Simulate Upstash daily quota (0=unlimited)
	--simulate-rps     N      Simulate Upstash RPS limit (0=unlimited)
	--localhost-only          Bind to 127.0.0.1 only (recommended on shared Wi-Fi)
	--disable-query-token     Reject ?_token= in URLs (use Authorization header)
	--cors-origin      ORIGIN CORS Allow-Origin (default: *)
	--require-dashboard-auth  Require auth for dashboard API (default: true)
	--block-dangerous-commands Block FLUSHALL, CONFIG, SHUTDOWN, etc.
	--secure                  Enable all hardening flags above
	--help                    Print this message

ENVIRONMENT VARIABLES:
	REDIS_ADDR                    Redis server address
	UPSTASH_ADDR                  Webserver address
	UPSTASH_TOKEN                 Full-access API token
	UPSTASH_READONLY_TOKEN        Read-only API token
	SIMULATE_QUOTA                Daily quota simulation
	SIMULATE_RPS                  RPS limit simulation
	UPSTASH_LOCALHOST_ONLY        Bind to 127.0.0.1 only
	UPSTASH_DISABLE_QUERY_TOKEN   Reject query-string tokens
	UPSTASH_CORS_ORIGIN           CORS origin (default: *)
	UPSTASH_REQUIRE_DASHBOARD_AUTH Require dashboard auth (default: true)
	UPSTASH_BLOCK_DANGEROUS_COMMANDS Block destructive Redis commands
	UPSTASH_SECURE                Enable all security hardening

ENDPOINTS:
	/health          Health check (no auth)
	/dashboard       Usage dashboard + key browser
	/pipeline        Command pipeline
	/multi-exec      Atomic transactions
	/monitor         SSE monitor stream
	/publish/:ch/:msg  Publish message
	/subscribe/:ch   SSE subscribe stream
`, Version)
}

func createRedisPool(addr string, maxRetries int, retryDelayMs int, logger *zap.Logger) *redis.Pool {
	return &redis.Pool{
		MaxIdle:     10,
		MaxActive:   100,
		IdleTimeout: 5 * time.Minute,
		Wait:        true,
		Dial: func() (redis.Conn, error) {
			return dialWithRetry(addr, maxRetries, retryDelayMs, logger)
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			if time.Since(t) < time.Minute {
				return nil
			}
			_, err := c.Do("PING")
			return err
		},
	}
}

func dialWithRetry(addr string, maxRetries int, retryDelayMs int, logger *zap.Logger) (redis.Conn, error) {
	var conn redis.Conn
	var err error

	for i := 0; i < maxRetries; i++ {
		conn, err = redis.Dial("tcp", addr,
			redis.DialConnectTimeout(5*time.Second),
			redis.DialReadTimeout(5*time.Second),
			redis.DialWriteTimeout(5*time.Second),
		)
		if err == nil {
			return conn, nil
		}

		logger.Warn("failed to connect to Redis, retrying...",
			zap.Int("attempt", i+1),
			zap.Int("maxRetries", maxRetries),
			zap.String("addr", addr),
			zap.Error(err),
		)

		delay := time.Duration(retryDelayMs*(1<<uint(i))) * time.Millisecond
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		time.Sleep(delay)
	}

	return nil, fmt.Errorf("failed to connect to Redis after %d attempts: %w", maxRetries, err)
}

func testRedisConnection(pool *redis.Pool, logger *zap.Logger) error {
	conn := pool.Get()
	defer conn.Close()

	_, err := conn.Do("PING")
	if err != nil {
		return fmt.Errorf("Redis PING failed: %w", err)
	}

	logger.Info("successfully connected to Redis")
	return nil
}
