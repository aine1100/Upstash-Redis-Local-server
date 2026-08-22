package internal

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// CommandLog keeps a fixed-size ring buffer of recent Redis commands for the dashboard.
type CommandLog struct {
	mu      sync.RWMutex
	entries []commandEntry
	max     int
}

type commandEntry struct {
	Time    int64  `json:"time"`
	Command string `json:"command"`
	Args    string `json:"args"`
}

func NewCommandLog(max int) *CommandLog {
	if max <= 0 {
		max = 100
	}
	return &CommandLog{max: max}
}

func (l *CommandLog) Record(command string, args []interface{}) {
	if l == nil {
		return
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = truncateStr(formatArg(a), 80)
	}
	argStr := strings.Join(parts, " ")

	l.mu.Lock()
	l.entries = append(l.entries, commandEntry{
		Time:    time.Now().UnixMilli(),
		Command: strings.ToUpper(command),
		Args:    argStr,
	})
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
	l.mu.Unlock()
}

func (l *CommandLog) Recent(limit int) []commandEntry {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := len(l.entries)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]commandEntry, limit)
	copy(out, l.entries[n-limit:])
	return out
}

func formatArg(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(v)
	}
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
