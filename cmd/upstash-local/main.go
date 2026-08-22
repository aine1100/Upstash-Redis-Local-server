package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	localURL      = "http://localhost:8000"
	localToken    = "local-dev-token"
	envFileName   = ".env.local"
	cloudEnvFile  = ".env.cloud"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "use":
		if len(os.Args) < 3 {
			fmt.Println("Usage: upstash-local use dev|cloud")
			os.Exit(1)
		}
		useProfile(os.Args[2])
	case "status":
		status()
	case "seed":
		fs := flag.NewFlagSet("seed", flag.ExitOnError)
		keys := fs.Int("keys", 100, "Number of keys to seed")
		prefix := fs.String("prefix", "dev:", "Key prefix")
		url := fs.String("url", localURL, "REST URL")
		token := fs.String("token", localToken, "API token")
		fs.Parse(os.Args[2:])
		seed(*url, *token, *prefix, *keys)
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		out := fs.String("output", "dump.json", "Output file")
		pattern := fs.String("pattern", "*", "Key pattern")
		url := fs.String("url", localURL, "REST URL")
		token := fs.String("token", localToken, "API token")
		fs.Parse(os.Args[2:])
		export(*url, *token, *pattern, *out)
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		input := fs.String("input", "dump.json", "Input file")
		url := fs.String("url", localURL, "REST URL")
		token := fs.String("token", localToken, "API token")
		fs.Parse(os.Args[2:])
		importData(*url, *token, *input)
	case "ping":
		url := localURL
		token := localToken
		if len(os.Args) > 2 {
			url = os.Args[2]
		}
		ping(url, token)
	case "generate-token":
		generateToken()
	case "replay":
		fs := flag.NewFlagSet("replay", flag.ExitOnError)
		input := fs.String("input", "session.jsonl", "Recorded JSONL file")
		url := fs.String("url", localURL, "REST URL")
		token := fs.String("token", localToken, "API token")
		fs.Parse(os.Args[2:])
		replay(*url, *token, *input)
	case "snapshot":
		if len(os.Args) < 3 {
			fmt.Println("Usage: upstash-local snapshot save|restore|list|delete")
			os.Exit(1)
		}
		snapshotCmd(os.Args[2:])
	case "pull":
		fs := flag.NewFlagSet("pull", flag.ExitOnError)
		cloudURL := fs.String("from", "", "Cloud Upstash REST URL (required)")
		cloudToken := fs.String("cloud-token", "", "Cloud REST token (required)")
		targetURL := fs.String("to", localURL, "Local REST URL")
		targetToken := fs.String("token", localToken, "Local API token")
		pattern := fs.String("pattern", "*", "Key pattern")
		mask := fs.Bool("mask-secrets", true, "Redact values matching secret/password/token")
		fs.Parse(os.Args[2:])
		pull(*cloudURL, *cloudToken, *targetURL, *targetToken, *pattern, *mask)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`upstash-local — manage local Upstash Redis dev environment

Commands:
  use dev          Switch app env to local (unlimited, no rate limits)
  use cloud        Switch app env to cloud credentials from .env.cloud
  status           Show active environment profile
  seed             Seed test keys (--keys 100 --prefix dev:)
  export           Export keys to JSON (--output dump.json)
  import           Import keys from JSON (--input dump.json)
  ping [url]       Test REST connection
  generate-token   Generate random local API tokens
  replay           Replay a recorded session (--input session.jsonl)
  snapshot         Named snapshots (save|restore|list|delete)
  pull             Pull keys from Upstash Cloud to local (--from URL --cloud-token TOKEN)`)
}

func replay(url, token, input string) {
	data, err := os.ReadFile(input)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", input, err)
		os.Exit(1)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	ok, failed := 0, 0
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var cmd []interface{}
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			fmt.Printf("  line %d: skipped (bad JSON)\n", i+1)
			failed++
			continue
		}
		_, code, err := restPost(url, token, cmd)
		if err != nil || code >= 400 {
			failed++
			continue
		}
		ok++
	}
	fmt.Printf("✅ Replayed %d commands (%d ok, %d failed) from %s\n", ok+failed, ok, failed, input)
}

func snapshotCmd(args []string) {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	name := fs.String("name", "", "Snapshot name")
	pattern := fs.String("pattern", "*", "Key pattern for save")
	url := fs.String("url", localURL, "REST URL")
	token := fs.String("token", localToken, "API token")
	fs.Parse(args[1:])

	sub := args[0]
	auth := restHeaders(*token)
	switch sub {
	case "list":
		req, _ := http.NewRequest("GET", *url+"/dashboard/api/snapshots", nil)
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Println(strings.TrimSpace(string(body)))
	case "save":
		if *name == "" {
			fmt.Println("--name required")
			os.Exit(1)
		}
		req, _ := http.NewRequest("POST", *url+"/dashboard/api/snapshots/"+*name+"?pattern="+urlQuery(*pattern), nil)
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("✅ Saved snapshot: %s\n", strings.TrimSpace(string(body)))
	case "restore":
		if *name == "" {
			fmt.Println("--name required")
			os.Exit(1)
		}
		req, _ := http.NewRequest("POST", *url+"/dashboard/api/snapshots/"+*name+"/restore", nil)
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("✅ Restored snapshot: %s\n", strings.TrimSpace(string(body)))
	case "delete":
		if *name == "" {
			fmt.Println("--name required")
			os.Exit(1)
		}
		req, _ := http.NewRequest("DELETE", *url+"/dashboard/api/snapshots/"+*name, nil)
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("✅ Deleted snapshot: %s\n", strings.TrimSpace(string(body)))
	default:
		fmt.Println("Usage: upstash-local snapshot save|restore|list|delete --name NAME")
		os.Exit(1)
	}
}

func pull(cloudURL, cloudToken, targetURL, targetToken, pattern string, maskSecrets bool) {
	if cloudURL == "" || cloudToken == "" {
		fmt.Println("--from and --cloud-token are required")
		os.Exit(1)
	}
	cloudURL = strings.TrimRight(cloudURL, "/")
	targetURL = strings.TrimRight(targetURL, "/")

	keys, err := scanCloudKeys(cloudURL, cloudToken, pattern)
	if err != nil {
		fmt.Printf("Error scanning cloud: %v\n", err)
		os.Exit(1)
	}

	var cmds [][]interface{}
	for _, key := range keys {
		val, err := cloudGet(cloudURL, cloudToken, key)
		if err != nil {
			continue
		}
		if maskSecrets && shouldMaskKey(key) {
			val = "[REDACTED]"
		}
		cmds = append(cmds, []interface{}{"SET", key, val})
	}
	if len(cmds) == 0 {
		fmt.Println("No keys pulled")
		return
	}
	data, code, err := restPost(targetURL+"/pipeline", targetToken, cmds)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Pulled %d keys to local (HTTP %d): %s\n", len(cmds), code, strings.TrimSpace(string(data)))
}

func scanCloudKeys(base, token, pattern string) ([]string, error) {
	var keys []string
	cursor := "0"
	for {
		req, _ := http.NewRequest("GET", base+"/SCAN/"+cursor+"/MATCH/"+pattern, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Result []interface{} `json:"result"`
		}
		json.Unmarshal(body, &out)
		if len(out.Result) < 2 {
			break
		}
		cursor = fmt.Sprint(out.Result[0])
		if batch, ok := out.Result[1].([]interface{}); ok {
			for _, k := range batch {
				keys = append(keys, fmt.Sprint(k))
			}
		}
		if cursor == "0" {
			break
		}
	}
	return keys, nil
}

func cloudGet(base, token, key string) (string, error) {
	req, _ := http.NewRequest("GET", base+"/GET/"+key, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Result string `json:"result"`
	}
	json.Unmarshal(body, &out)
	return out.Result, nil
}

func shouldMaskKey(key string) bool {
	lower := strings.ToLower(key)
	for _, s := range []string{"password", "secret", "token", "apikey", "api_key", "credential"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func restHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func urlQuery(s string) string {
	return url.QueryEscape(s)
}

func useProfile(name string) {
	dir, _ := os.Getwd()
	target := filepath.Join(dir, envFileName)

	switch name {
	case "dev", "local":
		content := fmt.Sprintf(`UPSTASH_REDIS_REST_URL=%s
UPSTASH_REDIS_REST_TOKEN=%s
UPSTASH_REDIS_REST_READONLY_TOKEN=local-readonly-token
`, localURL, localToken)
		if err := os.WriteFile(target, []byte(content), 0644); err != nil {
			fmt.Printf("Error writing %s: %v\n", target, err)
			os.Exit(1)
		}
		fmt.Printf("✅ Switched to LOCAL dev profile\n")
		fmt.Printf("   URL:   %s\n", localURL)
		fmt.Printf("   Token: %s\n", localToken)
		fmt.Println("   Unlimited requests — no cloud rate limits")
	case "cloud":
		cloudPath := filepath.Join(dir, cloudEnvFile)
		if _, err := os.Stat(cloudPath); os.IsNotExist(err) {
			fmt.Printf("Create %s with your cloud credentials first:\n", cloudEnvFile)
			fmt.Println("  UPSTASH_REDIS_REST_URL=https://xxx.upstash.io")
			fmt.Println("  UPSTASH_REDIS_REST_TOKEN=your-token")
			os.Exit(1)
		}
		data, _ := os.ReadFile(cloudPath)
		if err := os.WriteFile(target, data, 0644); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✅ Switched to CLOUD profile (from .env.cloud)")
	default:
		fmt.Println("Unknown profile. Use: dev or cloud")
		os.Exit(1)
	}
}

func status() {
	dir, _ := os.Getwd()
	path := filepath.Join(dir, envFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("No .env.local found. Run: upstash-local use dev")
		return
	}
	content := string(data)
	if strings.Contains(content, "localhost") {
		fmt.Println("Active profile: LOCAL (unlimited)")
	} else {
		fmt.Println("Active profile: CLOUD")
	}
	fmt.Println(strings.TrimSpace(content))
}

func restPost(url, token string, body interface{}) ([]byte, int, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

func ping(url, token string) {
	req, _ := http.NewRequest("GET", url+"/PING", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("❌ Connection failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("✅ %s → %s\n", url, strings.TrimSpace(string(body)))
}

func seed(url, token, prefix string, count int) {
	var cmds [][]interface{}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("%skey:%d", prefix, i)
		val := fmt.Sprintf("value-%d", i)
		cmds = append(cmds, []interface{}{"SET", key, val})
	}
	data, code, err := restPost(url+"/pipeline", token, cmds)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Seeded %d keys (HTTP %d): %s\n", count, code, strings.TrimSpace(string(data)))
}

func export(url, token, pattern, output string) {
	req, _ := http.NewRequest("GET", url+"/KEYS/"+pattern, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Result []string `json:"result"`
	}
	json.Unmarshal(body, &result)

	items := make([]map[string]interface{}, 0, len(result.Result))
	for _, key := range result.Result {
		getReq, _ := http.NewRequest("GET", url+"/GET/"+key, nil)
		getReq.Header.Set("Authorization", "Bearer "+token)
		getResp, err := http.DefaultClient.Do(getReq)
		if err != nil {
			continue
		}
		getBody, _ := io.ReadAll(getResp.Body)
		getResp.Body.Close()
		var getResult struct {
			Result interface{} `json:"result"`
		}
		json.Unmarshal(getBody, &getResult)
		items = append(items, map[string]interface{}{
			"key": key, "type": "string", "value": getResult.Result, "ttl": -1,
		})
	}

	out, _ := json.MarshalIndent(map[string]interface{}{"keys": items}, "", "  ")
	os.WriteFile(output, out, 0644)
	fmt.Printf("✅ Exported %d keys to %s\n", len(items), output)
}

func importData(url, token, input string) {
	data, err := os.ReadFile(input)
	if err != nil {
		fmt.Printf("Error reading %s: %v\n", input, err)
		os.Exit(1)
	}
	var dump struct {
		Keys []map[string]interface{} `json:"keys"`
	}
	json.Unmarshal(data, &dump)

	var cmds [][]interface{}
	for _, item := range dump.Keys {
		key, _ := item["key"].(string)
		val := item["value"]
		if key != "" {
			cmds = append(cmds, []interface{}{"SET", key, fmt.Sprint(val)})
		}
	}
	respData, code, err := restPost(url+"/pipeline", token, cmds)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Imported %d keys (HTTP %d): %s\n", len(cmds), code, strings.TrimSpace(string(respData)))
}

func generateToken() {
	full := randomToken("dev")
	readOnly := randomToken("ro")
	fmt.Println("Add these to your .env (never commit real cloud tokens):")
	fmt.Printf("UPSTASH_TOKEN=%s\n", full)
	fmt.Printf("UPSTASH_READONLY_TOKEN=%s\n", readOnly)
	fmt.Printf("UPSTASH_REDIS_REST_TOKEN=%s\n", full)
	fmt.Printf("UPSTASH_REDIS_REST_READONLY_TOKEN=%s\n", readOnly)
}

func randomToken(prefix string) string {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf[:])
}
