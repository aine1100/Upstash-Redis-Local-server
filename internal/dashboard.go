package internal

import (
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

	keys, err := redis.Strings(conn.Do("KEYS", pattern))
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

	keys, err := redis.Strings(conn.Do("KEYS", pattern))
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
</style>
</head>
<body>
<h1>Upstash Redis Local</h1>
<p class="sub">Unlimited local dev — no cloud rate limits</p>
<div class="grid" id="stats"></div>
<div class="toolbar">
  <input id="pattern" placeholder="Key pattern" value="*">
  <input id="token" placeholder="Token" value="local-dev-token">
  <button onclick="loadKeys()">Browse Keys</button>
  <button onclick="loadStats()">Refresh Stats</button>
</div>
<table>
  <thead><tr><th>Key</th><th>Type</th><th>TTL</th><th>Value</th></tr></thead>
  <tbody id="keys"></tbody>
</table>
<script>
function getToken() {
  const el = document.getElementById('token');
  const token = el.value.trim();
  if (token) sessionStorage.setItem('upstash_local_token', token);
  return token || sessionStorage.getItem('upstash_local_token') || '';
}
function authHeaders() {
  const token = getToken();
  return token ? { 'Authorization': 'Bearer ' + token } : {};
}
async function loadStats() {
  const r = await fetch('/dashboard/api/stats', { headers: authHeaders() });
  if (r.status === 401) {
    document.getElementById('stats').innerHTML = '<div class="card"><h3>Auth required</h3><div class="val" style="font-size:1rem">Enter your API token below</div></div>';
    return;
  }
  const d = await r.json();
  document.getElementById('stats').innerHTML =
    '<div class="card"><h3>Total Requests</h3><div class="val">'+(d.total_requests||0)+'</div></div>'+
    '<div class="card"><h3>Cloud Quota Saved</h3><div class="val green">'+(d.quota_saved||0)+'</div></div>'+
    '<div class="card"><h3>Free Tier Limit</h3><div class="val">'+(d.free_tier_quota||10000)+'/day</div></div>'+
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
(function(){
  const saved = sessionStorage.getItem('upstash_local_token');
  if (saved) document.getElementById('token').value = saved;
})();
loadStats(); setInterval(loadStats, 5000);
</script>
</body>
</html>`
