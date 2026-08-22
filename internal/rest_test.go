package internal_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
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
	return startTestServerOpts(t, func(s *internal.Server) { s.Security = sec })
}

func startTestServerOpts(t *testing.T, configure func(*internal.Server)) (*internal.Server, *miniredis.Miniredis, string) {
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
	commandACL := internal.NewCommandACL()
	commandACL.LoadFromRedis(pool, logger)
	server := &internal.Server{
		Address:       addr,
		APIToken:      "test-token",
		ReadOnlyToken: "readonly-token",
		RedisPool:     pool,
		Logger:        logger,
		Metrics:       internal.NewMetrics(),
		Security:      internal.SecurityConfig{RequireDashboardAuth: true},
		CommandLog:    internal.NewCommandLog(50),
		CommandACL:    commandACL,
		Dial: func() (redis.Conn, error) {
			return redis.Dial("tcp", mr.Addr())
		},
	}
	if configure != nil {
		configure(server)
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

func TestStrictUpstashBlocksCommand(t *testing.T) {
	_, mr, base := startTestServerWithSecurity(t, internal.SecurityConfig{
		StrictUpstash:        true,
		RequireDashboardAuth: true,
	})
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/SUBSCRIBE/chan", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected SUBSCRIBE blocked in strict mode, got %d", resp.StatusCode)
	}

	// A supported command still works.
	okReq, _ := http.NewRequest("GET", base+"/PING", nil)
	okReq.Header.Set("Authorization", "Bearer test-token")
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatal(err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected PING to work in strict mode, got %d", okResp.StatusCode)
	}
}

func TestChaosErrorInjection(t *testing.T) {
	_, mr, base := startTestServerOpts(t, func(s *internal.Server) {
		s.Chaos = &internal.ChaosConfig{ErrorRate: 1.0}
	})
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/PING", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected injected 500, got %d", resp.StatusCode)
	}
}

func TestChaosLatencyInjection(t *testing.T) {
	_, mr, base := startTestServerOpts(t, func(s *internal.Server) {
		s.Chaos = &internal.ChaosConfig{Latency: 150 * time.Millisecond}
	})
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/PING", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("expected at least 150ms latency, got %v", elapsed)
	}
}

func TestScanKeysDashboard(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	for _, k := range []string{"dev:a", "dev:b", "other:c"} {
		req, _ := http.NewRequest("GET", base+"/SET/"+k+"/v", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		http.DefaultClient.Do(req)
	}

	req, _ := http.NewRequest("GET", base+"/dashboard/api/keys?pattern=dev:*", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]interface{}
	json.Unmarshal(readBody(t, resp), &out)
	if out["count"].(float64) != 2 {
		t.Fatalf("expected 2 keys matching dev:*, got %v", out["count"])
	}
}

func TestReadOnlyAllowsScan(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/SCAN/0", nil)
	req.Header.Set("Authorization", "Bearer readonly-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected SCAN allowed for readonly token, got %d", resp.StatusCode)
	}
}

func TestDailyMetricsField(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	ping, _ := http.NewRequest("GET", base+"/PING", nil)
	ping.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(ping)

	req, _ := http.NewRequest("GET", base+"/dashboard/api/stats", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var stats map[string]interface{}
	json.Unmarshal(readBody(t, resp), &stats)
	if _, ok := stats["requests_today"]; !ok {
		t.Fatalf("expected requests_today in stats, got %v", stats)
	}
}

func TestQStashDelivers(t *testing.T) {
	received := make(chan string, 1)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer dest.Close()

	srv, mr, base := startTestServerOpts(t, func(s *internal.Server) {
		q := internal.NewQStash(s.RedisPool, s.Logger)
		q.SetPollInterval(50 * time.Millisecond)
		s.QStash = q
	})
	defer mr.Close()
	srv.QStash.Start()
	defer srv.QStash.Stop()

	publishURL := base + "/v2/publish/" + url.QueryEscape(dest.URL)
	req, _ := http.NewRequest("POST", publishURL, strings.NewReader(`{"email":"hi"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on publish, got %d", resp.StatusCode)
	}
	var pub map[string]string
	json.Unmarshal(readBody(t, resp), &pub)
	if pub["messageId"] == "" {
		t.Fatal("expected a messageId in publish response")
	}

	select {
	case body := <-received:
		if body != `{"email":"hi"}` {
			t.Fatalf("destination got wrong body: %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message was not delivered within 3s")
	}
}

func TestQStashDeadLetterQueue(t *testing.T) {
	// Destination always fails, so the message should land in the DLQ.
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dest.Close()

	srv, mr, base := startTestServerOpts(t, func(s *internal.Server) {
		q := internal.NewQStash(s.RedisPool, s.Logger)
		q.SetPollInterval(20 * time.Millisecond)
		s.QStash = q
	})
	defer mr.Close()
	srv.QStash.Start()
	defer srv.QStash.Stop()

	publishURL := base + "/v2/publish/" + url.QueryEscape(dest.URL)
	req, _ := http.NewRequest("POST", publishURL, strings.NewReader(`x`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Upstash-Retries", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("message never reached the DLQ")
		default:
		}
		dlqReq, _ := http.NewRequest("GET", base+"/v2/dlq", nil)
		dlqReq.Header.Set("Authorization", "Bearer test-token")
		dlqResp, err := http.DefaultClient.Do(dlqReq)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]interface{}
		json.Unmarshal(readBody(t, dlqResp), &out)
		dlqResp.Body.Close()
		if count, ok := out["count"].(float64); ok && count >= 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRecorder(t *testing.T) {
	recPath := t.TempDir() + "/session.jsonl"
	rec, err := internal.NewRecorder(recPath)
	if err != nil {
		t.Fatal(err)
	}

	_, mr, base := startTestServerOpts(t, func(s *internal.Server) {
		s.Recorder = rec
	})
	defer mr.Close()

	for _, path := range []string{"/SET/foo/bar", "/GET/foo"} {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	rec.Close()

	data, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 recorded commands, got %d: %q", len(lines), string(data))
	}
	if !strings.Contains(lines[0], "SET") || !strings.Contains(lines[1], "GET") {
		t.Fatalf("recorded commands unexpected: %v", lines)
	}
}

func TestSnapshotSaveRestore(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	setReq, _ := http.NewRequest("GET", base+"/SET/snap:a/1", nil)
	setReq.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(setReq)

	saveReq, _ := http.NewRequest("POST", base+"/dashboard/api/snapshots/users-test?pattern=snap:*", nil)
	saveReq.Header.Set("Authorization", "Bearer test-token")
	saveResp, err := http.DefaultClient.Do(saveReq)
	if err != nil {
		t.Fatal(err)
	}
	saveResp.Body.Close()

	delReq, _ := http.NewRequest("GET", base+"/DEL/snap:a", nil)
	delReq.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(delReq)

	restoreReq, _ := http.NewRequest("POST", base+"/dashboard/api/snapshots/users-test/restore", nil)
	restoreReq.Header.Set("Authorization", "Bearer test-token")
	restoreResp, err := http.DefaultClient.Do(restoreReq)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("restore failed: %d", restoreResp.StatusCode)
	}

	getReq, _ := http.NewRequest("GET", base+"/GET/snap:a", nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	body, _ := io.ReadAll(getResp.Body)
	if !strings.Contains(string(body), "1") {
		t.Fatalf("expected restored value, got %s", body)
	}
}

func TestCommandLogMonitor(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	ping, _ := http.NewRequest("GET", base+"/PING", nil)
	ping.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(ping)

	req, _ := http.NewRequest("GET", base+"/dashboard/api/monitor", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	json.Unmarshal(readBody(t, resp), &out)
	if out["count"].(float64) < 1 {
		t.Fatalf("expected command log entries, got %v", out)
	}
}

func TestQStashGetMessageAndReplay(t *testing.T) {
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dest.Close()

	srv, mr, base := startTestServerOpts(t, func(s *internal.Server) {
		q := internal.NewQStash(s.RedisPool, s.Logger)
		q.SetPollInterval(30 * time.Millisecond)
		s.QStash = q
	})
	defer mr.Close()
	srv.QStash.Start()
	defer srv.QStash.Stop()

	pubURL := base + "/v2/publish/?url=" + url.QueryEscape(dest.URL)
	pub, _ := http.NewRequest("POST", pubURL, strings.NewReader("x"))
	pub.Header.Set("Authorization", "Bearer test-token")
	pub.Header.Set("Upstash-Retries", "0")
	http.DefaultClient.Do(pub)

	time.Sleep(500 * time.Millisecond)

	listReq, _ := http.NewRequest("GET", base+"/v2/messages", nil)
	listReq.Header.Set("Authorization", "Bearer test-token")
	listResp, _ := http.DefaultClient.Do(listReq)
	var listed map[string]interface{}
	json.Unmarshal(readBody(t, listResp), &listed)
	listResp.Body.Close()
	msgs := listed["messages"].([]interface{})
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	msgID := msgs[0].(map[string]interface{})["messageId"].(string)

	getReq, _ := http.NewRequest("GET", base+"/v2/messages/"+msgID, nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get message expected 200, got %d", getResp.StatusCode)
	}

	dlqReq, _ := http.NewRequest("GET", base+"/v2/dlq", nil)
	dlqReq.Header.Set("Authorization", "Bearer test-token")
	dlqResp, _ := http.DefaultClient.Do(dlqReq)
	var dlq map[string]interface{}
	json.Unmarshal(readBody(t, dlqResp), &dlq)
	dlqResp.Body.Close()
	if dlq["count"].(float64) < 1 {
		t.Fatal("expected message in DLQ before replay")
	}

	replayReq, _ := http.NewRequest("POST", base+"/v2/dlq/"+msgID+"/replay", nil)
	replayReq.Header.Set("Authorization", "Bearer test-token")
	replayResp, err := http.DefaultClient.Do(replayReq)
	if err != nil {
		t.Fatal(err)
	}
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusOK {
		t.Fatalf("dlq replay expected 200, got %d", replayResp.StatusCode)
	}
}

func TestCommandACLBlocksHSET(t *testing.T) {
	_, mr, base := startTestServer(t)
	defer mr.Close()

	req, _ := http.NewRequest("GET", base+"/HSET/myhash/f/v", nil)
	req.Header.Set("Authorization", "Bearer readonly-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HSET blocked for readonly, got %d", resp.StatusCode)
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
