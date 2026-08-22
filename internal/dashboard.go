package internal

import (
	"encoding/json"
	"fmt"

	"github.com/gomodule/redigo/redis"
	"github.com/valyala/fasthttp"
)

func (s *Server) handleDashboard(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("text/html; charset=utf-8")
	ctx.SetBody([]byte(dashboardHTML))
}

func (s *Server) handleDashboardStats(ctx *fasthttp.RequestCtx) {
	if s.Metrics == nil {
		s.writeJSON(ctx, map[string]interface{}{"total_requests": 0}, fasthttp.StatusOK)
		return
	}
	s.writeJSON(ctx, s.Metrics.Snapshot(), fasthttp.StatusOK)
}

func (s *Server) handleDashboardKeys(ctx *fasthttp.RequestCtx) {
	pattern := string(ctx.QueryArgs().Peek("pattern"))
	if pattern == "" {
		pattern = "*"
	}

	conn := s.RedisPool.Get()
	defer conn.Close()

	keys, err := scanKeys(conn, pattern, 1000)
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}

	type keyInfo struct {
		Key   string      `json:"key"`
		Type  string      `json:"type"`
		Value interface{} `json:"value"`
		TTL   int64       `json:"ttl"`
	}

	items := make([]keyInfo, 0, len(keys))
	for _, key := range keys {
		keyType, _ := redis.String(conn.Do("TYPE", key))
		ttl, _ := redis.Int64(conn.Do("TTL", key))
		value := readKeyValue(conn, key, keyType)
		items = append(items, keyInfo{Key: key, Type: keyType, Value: value, TTL: ttl})
	}

	s.writeJSON(ctx, map[string]interface{}{"keys": items, "count": len(items)}, fasthttp.StatusOK)
}

func (s *Server) handleDashboardMonitor(ctx *fasthttp.RequestCtx) {
	limit := ctx.QueryArgs().GetUintOrZero("limit")
	if s.CommandLog == nil {
		s.writeJSON(ctx, map[string]interface{}{"entries": []commandEntry{}, "count": 0}, fasthttp.StatusOK)
		return
	}
	entries := s.CommandLog.Recent(int(limit))
	s.writeJSON(ctx, map[string]interface{}{"entries": entries, "count": len(entries)}, fasthttp.StatusOK)
}

func (s *Server) handleDashboardExecute(ctx *fasthttp.RequestCtx) {
	var payload struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil || payload.Command == "" {
		s.writeJSON(ctx, errorResult{Error: "command required"}, fasthttp.StatusBadRequest)
		return
	}
	args := make([]interface{}, len(payload.Args))
	for i, a := range payload.Args {
		args[i] = a
	}
	auth := &authResult{creds: credentials{}}
	res, code := s.executeCommand(auth, payload.Command, args...)
	s.writeJSON(ctx, res, code)
}

// scanKeys iterates keys with a non-blocking SCAN cursor instead of KEYS.
// limit <= 0 returns everything; otherwise it stops after roughly limit keys.
func scanKeys(conn redis.Conn, pattern string, limit int) ([]string, error) {
	if pattern == "" {
		pattern = "*"
	}
	var keys []string
	cursor := 0
	for {
		vals, err := redis.Values(conn.Do("SCAN", cursor, "MATCH", pattern, "COUNT", 200))
		if err != nil {
			return nil, err
		}
		cursor, _ = redis.Int(vals[0], nil)
		batch, _ := redis.Strings(vals[1], nil)
		keys = append(keys, batch...)
		if limit > 0 && len(keys) >= limit {
			keys = keys[:limit]
			break
		}
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

func readKeyValue(conn redis.Conn, key, keyType string) interface{} {
	switch keyType {
	case "string":
		v, _ := redis.String(conn.Do("GET", key))
		return v
	case "hash":
		v, _ := redis.StringMap(conn.Do("HGETALL", key))
		return v
	case "list":
		v, _ := redis.Strings(conn.Do("LRANGE", key, 0, 49))
		return v
	case "set":
		v, _ := redis.Strings(conn.Do("SMEMBERS", key))
		return v
	case "zset":
		v, _ := redis.Strings(conn.Do("ZRANGE", key, 0, 49, "WITHSCORES"))
		return v
	default:
		return nil
	}
}

// ExportData returns all keys for CLI export.
func (s *Server) ExportData(pattern string) ([]map[string]interface{}, error) {
	conn := s.RedisPool.Get()
	defer conn.Close()

	keys, err := scanKeys(conn, pattern, 0)
	if err != nil {
		return nil, err
	}

	items := make([]map[string]interface{}, 0, len(keys))
	for _, key := range keys {
		keyType, _ := redis.String(conn.Do("TYPE", key))
		ttl, _ := redis.Int64(conn.Do("TTL", key))
		items = append(items, map[string]interface{}{
			"key":   key,
			"type":  keyType,
			"value": readKeyValue(conn, key, keyType),
			"ttl":   ttl,
		})
	}
	return items, nil
}

// ImportData loads exported keys into Redis.
func (s *Server) ImportData(items []map[string]interface{}) error {
	conn := s.RedisPool.Get()
	defer conn.Close()

	for _, item := range items {
		key, _ := item["key"].(string)
		keyType, _ := item["type"].(string)
		if key == "" {
			continue
		}
		if err := writeKeyValue(conn, key, keyType, item["value"]); err != nil {
			return fmt.Errorf("import %s: %w", key, err)
		}
		if ttl, ok := item["ttl"].(float64); ok && int64(ttl) > 0 {
			conn.Do("EXPIRE", key, int64(ttl))
		}
	}
	return nil
}

func writeKeyValue(conn redis.Conn, key, keyType string, value interface{}) error {
	switch keyType {
	case "string":
		_, err := conn.Do("SET", key, fmt.Sprint(value))
		return err
	case "hash":
		m, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid hash value")
		}
		args := redis.Args{}.Add(key)
		for k, v := range m {
			args = args.Add(k, fmt.Sprint(v))
		}
		_, err := conn.Do("HSET", args...)
		return err
	default:
		if s := fmt.Sprint(value); s != "" && s != "<nil>" {
			_, err := conn.Do("SET", key, s)
			return err
		}
	}
	return nil
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Upstash Redis Local — Dashboard</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: system-ui, sans-serif; background: #0f172a; color: #e2e8f0; padding: 2rem; }
  h1 { font-size: 1.5rem; margin-bottom: .25rem; }
  .sub { color: #94a3b8; margin-bottom: 2rem; font-size: .9rem; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 1rem; margin-bottom: 2rem; }
  .card { background: #1e293b; border-radius: 12px; padding: 1.25rem; border: 1px solid #334155; }
  .card h3 { font-size: .75rem; text-transform: uppercase; color: #94a3b8; margin-bottom: .5rem; }
  .card .val { font-size: 2rem; font-weight: 700; color: #38bdf8; }
  .card .val.green { color: #4ade80; }
  table { width: 100%; border-collapse: collapse; background: #1e293b; border-radius: 12px; overflow: hidden; }
  th, td { padding: .75rem 1rem; text-align: left; border-bottom: 1px solid #334155; font-size: .875rem; }
  th { background: #334155; color: #94a3b8; font-size: .75rem; text-transform: uppercase; }
  tr:hover td { background: #263248; }
  .badge { background: #334155; padding: .2rem .5rem; border-radius: 4px; font-size: .75rem; }
  input { background: #1e293b; border: 1px solid #475569; color: #e2e8f0; padding: .5rem .75rem; border-radius: 8px; margin-right: .5rem; }
  button { background: #38bdf8; color: #0f172a; border: none; padding: .5rem 1rem; border-radius: 8px; cursor: pointer; font-weight: 600; }
  .toolbar { margin-bottom: 1rem; display: flex; gap: .5rem; flex-wrap: wrap; align-items: center; }
  .tabs { display: flex; gap: .5rem; margin-bottom: 1rem; }
  .tab { background: #334155; color: #e2e8f0; border: none; padding: .5rem 1rem; border-radius: 8px; cursor: pointer; }
  .tab.active { background: #38bdf8; color: #0f172a; font-weight: 600; }
  .panel { display: none; } .panel.active { display: block; }
  .mono { font-family: ui-monospace, monospace; font-size: .8rem; }
  pre { background: #1e293b; padding: 1rem; border-radius: 8px; overflow: auto; max-height: 300px; }
</style>
</head>
<body>
<h1>Upstash Redis Local</h1>
<p class="sub">Unlimited local dev — no cloud rate limits</p>
<div class="grid" id="stats"></div>
<div class="toolbar">
  <input id="token" placeholder="API token" value="local-dev-token" style="min-width:200px">
  <button onclick="loadStats()">Refresh Stats</button>
</div>
<div class="tabs">
  <button class="tab active" onclick="showTab('keys')">Keys</button>
  <button class="tab" onclick="showTab('monitor')">Live Monitor</button>
  <button class="tab" onclick="showTab('qstash')">QStash</button>
  <button class="tab" onclick="showTab('snapshots')">Snapshots</button>
  <button class="tab" onclick="showTab('cli')">Run Command</button>
</div>
<div id="panel-keys" class="panel active">
  <div class="toolbar">
    <input id="pattern" placeholder="Key pattern" value="*">
    <button onclick="loadKeys()">Browse Keys</button>
  </div>
  <table><thead><tr><th>Key</th><th>Type</th><th>TTL</th><th>Value</th></tr></thead><tbody id="keys"></tbody></table>
</div>
<div id="panel-monitor" class="panel">
  <button onclick="loadMonitor()">Refresh</button>
  <pre id="monitor" class="mono"></pre>
</div>
<div id="panel-qstash" class="panel">
  <button onclick="loadQStash()">Refresh Messages</button>
  <button onclick="loadDLQ()">Refresh DLQ</button>
  <h3 style="margin:1rem 0 .5rem">Messages</h3>
  <pre id="qstash-msgs" class="mono"></pre>
  <h3 style="margin:1rem 0 .5rem">Dead Letter Queue</h3>
  <pre id="qstash-dlq" class="mono"></pre>
</div>
<div id="panel-snapshots" class="panel">
  <div class="toolbar">
    <input id="snap-name" placeholder="Snapshot name">
    <input id="snap-pattern" placeholder="Pattern" value="*">
    <button onclick="saveSnapshot()">Save</button>
    <button onclick="listSnapshots()">List</button>
  </div>
  <pre id="snapshots" class="mono"></pre>
</div>
<div id="panel-cli" class="panel">
  <div class="toolbar">
    <input id="cmd" placeholder="Command e.g. GET" style="width:120px">
    <input id="cmd-args" placeholder="Args space-separated" style="min-width:240px">
    <button onclick="runCommand()">Run</button>
  </div>
  <pre id="cmd-out" class="mono"></pre>
</div>
<script>
function showTab(name) {
  document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
  event.target.classList.add('active');
  document.getElementById('panel-'+name).classList.add('active');
}
function getToken() {
  const el = document.getElementById('token');
  const token = el.value.trim();
  if (token) sessionStorage.setItem('upstash_local_token', token);
  return token || sessionStorage.getItem('upstash_local_token') || '';
}
function authHeaders(json) {
  const h = { 'Authorization': 'Bearer ' + getToken() };
  if (json) h['Content-Type'] = 'application/json';
  return h;
}
async function loadStats() {
  const r = await fetch('/dashboard/api/stats', { headers: authHeaders() });
  if (r.status === 401) {
    document.getElementById('stats').innerHTML = '<div class="card"><h3>Auth required</h3><div class="val" style="font-size:1rem">Enter your API token</div></div>';
    return;
  }
  const d = await r.json();
  document.getElementById('stats').innerHTML =
    '<div class="card"><h3>Total Requests</h3><div class="val">'+(d.total_requests||0)+'</div></div>'+
    '<div class="card"><h3>Today</h3><div class="val green">'+(d.requests_today||0)+'</div></div>'+
    '<div class="card"><h3>Quota Saved</h3><div class="val green">'+(d.quota_saved||0)+'</div></div>'+
    '<div class="card"><h3>Uptime</h3><div class="val" style="font-size:1.2rem">'+(d.uptime_seconds||0)+'s</div></div>';
}
async function loadKeys() {
  const pattern = document.getElementById('pattern').value;
  const r = await fetch('/dashboard/api/keys?pattern='+encodeURIComponent(pattern), { headers: authHeaders() });
  const d = await r.json();
  const tbody = document.getElementById('keys');
  if (d.error) { tbody.innerHTML = '<tr><td colspan="4">'+d.error+'</td></tr>'; return; }
  tbody.innerHTML = (d.keys||[]).map(k => '<tr><td>'+k.key+'</td><td><span class="badge">'+k.type+'</span></td><td>'+(k.ttl<0?'∞':k.ttl+'s')+'</td><td>'+JSON.stringify(k.value).slice(0,80)+'</td></tr>').join('');
}
async function loadMonitor() {
  const r = await fetch('/dashboard/api/monitor?limit=50', { headers: authHeaders() });
  const d = await r.json();
  document.getElementById('monitor').textContent = (d.entries||[]).map(e => new Date(e.time).toISOString().slice(11,19)+' '+e.command+' '+e.args).join('\n') || '(no commands yet)';
}
async function loadQStash() {
  const r = await fetch('/v2/messages', { headers: authHeaders() });
  document.getElementById('qstash-msgs').textContent = JSON.stringify(await r.json(), null, 2);
}
async function loadDLQ() {
  const r = await fetch('/v2/dlq', { headers: authHeaders() });
  document.getElementById('qstash-dlq').textContent = JSON.stringify(await r.json(), null, 2);
}
async function listSnapshots() {
  const r = await fetch('/dashboard/api/snapshots', { headers: authHeaders() });
  document.getElementById('snapshots').textContent = JSON.stringify(await r.json(), null, 2);
}
async function saveSnapshot() {
  const name = document.getElementById('snap-name').value.trim();
  const pattern = document.getElementById('snap-pattern').value || '*';
  if (!name) return;
  const r = await fetch('/dashboard/api/snapshots/'+encodeURIComponent(name)+'?pattern='+encodeURIComponent(pattern), { method: 'POST', headers: authHeaders() });
  document.getElementById('snapshots').textContent = JSON.stringify(await r.json(), null, 2);
}
async function runCommand() {
  const cmd = document.getElementById('cmd').value.trim();
  const args = document.getElementById('cmd-args').value.trim().split(/\s+/).filter(Boolean);
  const r = await fetch('/dashboard/api/execute', { method: 'POST', headers: authHeaders(true), body: JSON.stringify({ command: cmd, args }) });
  document.getElementById('cmd-out').textContent = JSON.stringify(await r.json(), null, 2);
}
(function(){
  const saved = sessionStorage.getItem('upstash_local_token');
  if (saved) document.getElementById('token').value = saved;
})();
loadStats(); setInterval(loadStats, 5000); setInterval(loadMonitor, 3000);
</script>
</body>
</html>`
