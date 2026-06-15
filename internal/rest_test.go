package internal_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"
	"upstash-redis-local/internal"
)

var testPortCounter atomic.Int32

func startTestServer(t *testing.T) (*internal.Server, *miniredis.Miniredis, string) {
	return startTestServerWithSecurity(t, internal.SecurityConfig{RequireDashboardAuth: true})
}

func startTestServerWithSecurity(t *testing.T, sec internal.SecurityConfig) (*internal.Server, *miniredis.Miniredis, string) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}

	pool := &redis.Pool{
		Dial: func() (redis.Conn, error) {
			return redis.Dial("tcp", mr.Addr())
		},
	}

	port := 18080 + testPortCounter.Add(1)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	logger, _ := zap.NewDevelopment()
	server := &internal.Server{
		Address:       addr,
		APIToken:      "test-token",
		ReadOnlyToken: "readonly-token",
		RedisPool:     pool,
		Logger:        logger,
		Metrics:       internal.NewMetrics(),
		Security:      sec,
	}

	go server.Serve()
	time.Sleep(200 * time.Millisecond)

	return server, mr, "http://" + addr
}

func TestHealth(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestPing(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/PING", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	if result["result"] != "PONG" {
		t.Fatalf("expected PONG, got %v", result)
	}
}

func TestSetGet(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	setReq, _ := http.NewRequest("GET", base+"/SET/mykey/myvalue", nil)
	setReq.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(setReq)

	getReq, _ := http.NewRequest("GET", base+"/GET/mykey", nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	json.Unmarshal(body, &result)
	if result["result"] != "myvalue" {
		t.Fatalf("expected myvalue, got %v", result)
	}
}

func TestPipeline(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	payload := `[["SET","a","1"],["GET","a"]]`
	req, _ := http.NewRequest("POST", base+"/pipeline", io.NopCloser(jsonReader(payload)))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMultiExec(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	payload := `[["SET","tx1","val1"],["GET","tx1"]]`
	req, _ := http.NewRequest("POST", base+"/multi-exec", io.NopCloser(jsonReader(payload)))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestReadOnlyToken(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/SET/readonly/key", nil)
	req.Header.Set("Authorization", "Bearer readonly-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected write to be blocked for readonly token, got %d", resp.StatusCode)
	}
}

func TestUnauthorized(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	resp, err := http.Get(base + "/PING")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestDashboardStats(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	pingReq, _ := http.NewRequest("GET", base+"/PING", nil)
	pingReq.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(pingReq)

	req, _ := http.NewRequest("GET", base+"/dashboard/api/stats", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats map[string]interface{}
	json.Unmarshal(readBody(t, resp), &stats)
	if stats["total_requests"].(float64) < 1 {
		t.Fatalf("expected at least 1 request recorded, got %v", stats)
	}
}

func TestDashboardStatsUnauthorized(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	resp, err := http.Get(base + "/dashboard/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestDisableQueryToken(t *testing.T) {
	_, mr, base := startTestServerWithSecurity(t, internal.SecurityConfig{
		DisableQueryToken:    true,
		RequireDashboardAuth: true,
	})
	defer mr.Close()

	resp, err := http.Get(base + "/PING?_token=test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected query token to be rejected, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", base+"/PING", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected header auth to work, got %d", resp.StatusCode)
	}
}

func TestBlockDangerousCommands(t *testing.T) {
	_, mr, base := startTestServerWithSecurity(t, internal.SecurityConfig{
		BlockDangerousCommands: true,
		RequireDashboardAuth:   true,
	})
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/FLUSHALL", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected FLUSHALL to be blocked, got %d", resp.StatusCode)
	}
}

func TestResolveListenAddr(t *testing.T) {
	if got := internal.ResolveListenAddr(":8000", true); got != "127.0.0.1:8000" {
		t.Fatalf("expected 127.0.0.1:8000, got %s", got)
	}
	if got := internal.ResolveListenAddr("0.0.0.0:8000", true); got != "127.0.0.1:8000" {
		t.Fatalf("expected 127.0.0.1:8000, got %s", got)
	}
}

func jsonReader(s string) io.Reader {
	return io.NopCloser(&jsonBuffer{s: s, i: 0})
}

type jsonBuffer struct {
	s string
	i int
}

func (b *jsonBuffer) Read(p []byte) (int, error) {
	if b.i >= len(b.s) {
		return 0, io.EOF
	}
	n := copy(p, b.s[b.i:])
	b.i += n
	return n, nil
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
