package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"upstash-redis-local/internal"
)

var Version = "development"

type Cmd struct {
	RedisAddr    string
	Addr         string
	ApiToken     string
	MaxRetries   int
	RetryDelayMs int
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

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	setupFlags(flag.CommandLine)
	
	// Support environment variables for Docker configuration
	defaultRedis := getEnvOrDefault("REDIS_ADDR", ":6379")
	defaultAddr := getEnvOrDefault("UPSTASH_ADDR", ":8000")
	defaultToken := getEnvOrDefault("UPSTASH_TOKEN", "upstash")
	
	redisAddr := flag.String("redis", defaultRedis, "Redis server address (env: REDIS_ADDR)")
	addr := flag.String("addr", defaultAddr, "Webserver address (env: UPSTASH_ADDR)")
	apiToken := flag.String("token", defaultToken, "API token (env: UPSTASH_TOKEN)")
	maxRetries := flag.Int("max-retries", 10, "Max connection retries on startup")
	retryDelay := flag.Int("retry-delay", 1000, "Delay between retries in milliseconds")
	help := flag.Bool("help", false, "Print help message")
	flag.Parse()
	
	cmd := Cmd{
		RedisAddr:    *redisAddr,
		ApiToken:     *apiToken,
		Addr:         *addr,
		MaxRetries:   *maxRetries,
		RetryDelayMs: *retryDelay,
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

	// Create connection pool with retry logic
	pool := createRedisPool(cmd.RedisAddr, cmd.MaxRetries, cmd.RetryDelayMs, logger)
	defer pool.Close()

	// Test initial connection
	if err := testRedisConnection(pool, logger); err != nil {
		log.Fatalf("failed to connect to Redis: %v", err)
	}

	server := internal.Server{
		Address:   cmd.Addr,
		APIToken:  cmd.ApiToken,
		RedisPool: pool,
		Logger:    logger,
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

       * Connect to any local redis of your choice for testing
       * Completely mimics the upstash REST API https://docs.upstash.com/redis/features/restapi

Developed by Hemanth Krishna (https://github.com/DarthBenro008)

USAGE:
	upstash-redis-local
	upstash-redis-local --token upstash --addr :8000 --redis :6379

ARGUMENTS:
	--token       TOKEN  The API token to accept as authorised (default: upstash)
	--addr        ADDR   Address for the server to listen on (default: :8000)
	--redis       ADDR   Address to your redis server (default: :6379)
	--max-retries N      Max connection retries on startup (default: 10)
	--retry-delay MS     Delay between retries in milliseconds (default: 1000)
	--help               Prints this message

ENVIRONMENT VARIABLES:
	REDIS_ADDR     Redis server address (alternative to --redis)
	UPSTASH_ADDR   Webserver address (alternative to --addr)
	UPSTASH_TOKEN  API token (alternative to --token)
`, Version)
}

// createRedisPool creates a connection pool with proper configuration
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

// dialWithRetry attempts to connect to Redis with exponential backoff
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
		
		// Exponential backoff with cap
		delay := time.Duration(retryDelayMs*(1<<uint(i))) * time.Millisecond
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		time.Sleep(delay)
	}
	
	return nil, fmt.Errorf("failed to connect to Redis after %d attempts: %w", maxRetries, err)
}

// testRedisConnection verifies the pool can connect to Redis
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
