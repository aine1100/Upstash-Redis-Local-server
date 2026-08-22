package internal

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gomodule/redigo/redis"
	"github.com/valyala/fasthttp"
)

const (
	snapshotIndexKey = "snapshots:index"
	snapshotDataKey  = "snapshots:data:"
)

type snapshotMeta struct {
	Name      string `json:"name"`
	KeyCount  int    `json:"keyCount"`
	CreatedAt int64  `json:"createdAt"`
	Pattern   string `json:"pattern"`
}

func snapshotDataRedisKey(name string) string {
	return snapshotDataKey + name
}

func (s *Server) saveSnapshot(name, pattern string) (snapshotMeta, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return snapshotMeta{}, fmt.Errorf("snapshot name required")
	}
	items, err := s.ExportData(pattern)
	if err != nil {
		return snapshotMeta{}, err
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return snapshotMeta{}, err
	}

	conn := s.RedisPool.Get()
	defer conn.Close()
	if _, err := conn.Do("SET", snapshotDataRedisKey(name), payload); err != nil {
		return snapshotMeta{}, err
	}
	conn.Do("SADD", snapshotIndexKey, name)

	meta := snapshotMeta{
		Name:     name,
		KeyCount: len(items),
		Pattern:  pattern,
	}
	return meta, nil
}

func (s *Server) listSnapshots() ([]string, error) {
	conn := s.RedisPool.Get()
	defer conn.Close()
	return redis.Strings(conn.Do("SMEMBERS", snapshotIndexKey))
}

func (s *Server) restoreSnapshot(name string) (int, error) {
	conn := s.RedisPool.Get()
	defer conn.Close()

	raw, err := redis.Bytes(conn.Do("GET", snapshotDataRedisKey(name)))
	if err != nil {
		return 0, fmt.Errorf("snapshot %q not found", name)
	}
	var items []map[string]interface{}
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0, err
	}
	if err := s.ImportData(items); err != nil {
		return 0, err
	}
	return len(items), nil
}

func (s *Server) deleteSnapshot(name string) error {
	conn := s.RedisPool.Get()
	defer conn.Close()
	conn.Do("SREM", snapshotIndexKey, name)
	_, err := conn.Do("DEL", snapshotDataRedisKey(name))
	return err
}

func (s *Server) handleSnapshotList(ctx *fasthttp.RequestCtx) {
	names, err := s.listSnapshots()
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}
	s.writeJSON(ctx, map[string]interface{}{"snapshots": names, "count": len(names)}, fasthttp.StatusOK)
}

func (s *Server) handleSnapshotSave(ctx *fasthttp.RequestCtx, name string) {
	pattern := string(ctx.QueryArgs().Peek("pattern"))
	if pattern == "" {
		pattern = "*"
	}
	meta, err := s.saveSnapshot(name, pattern)
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}
	s.writeJSON(ctx, meta, fasthttp.StatusOK)
}

func (s *Server) handleSnapshotRestore(ctx *fasthttp.RequestCtx, name string) {
	count, err := s.restoreSnapshot(name)
	if err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}
	s.writeJSON(ctx, map[string]interface{}{"restored": count, "name": name}, fasthttp.StatusOK)
}

func (s *Server) handleSnapshotDelete(ctx *fasthttp.RequestCtx, name string) {
	if err := s.deleteSnapshot(name); err != nil {
		s.writeJSON(ctx, errorResult{Error: err.Error()}, fasthttp.StatusBadRequest)
		return
	}
	s.writeJSON(ctx, map[string]string{"deleted": name}, fasthttp.StatusOK)
}
