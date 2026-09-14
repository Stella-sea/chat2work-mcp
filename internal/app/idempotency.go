package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type requestIDCache struct {
	mu      sync.Mutex
	entries map[string]*requestIDEntry
	ttl     time.Duration
	limit   int
}

type requestIDEntry struct {
	done        chan struct{}
	result      []byte
	err         error
	completedAt time.Time
}

func newRequestIDCache(ttl time.Duration, limit int) *requestIDCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if limit <= 0 {
		limit = 1000
	}
	return &requestIDCache{entries: make(map[string]*requestIDEntry), ttl: ttl, limit: limit}
}

// call returns a private result copy. Calls with the same authenticated key
// share an in-flight execution and cache only completed MCP results.
func (c *requestIDCache) call(key string, execute func() (*mcp.CallToolResult, any, error)) (*mcp.CallToolResult, any, error, bool) {
	for {
		c.mu.Lock()
		c.removeExpired(time.Now())
		if entry, ok := c.entries[key]; ok {
			if entry.completedAt.IsZero() {
				done := entry.done
				c.mu.Unlock()
				<-done
				result, err := decodeCallToolResult(entry.result)
				if err == nil {
					return result, nil, entry.err, true
				}
				continue
			}
			result, err := decodeCallToolResult(entry.result)
			c.mu.Unlock()
			if err == nil {
				return result, nil, nil, true
			}
			// A result that cannot be safely copied is not useful as a cache entry.
			c.mu.Lock()
			delete(c.entries, key)
			c.mu.Unlock()
			continue
		}
		if len(c.entries) >= c.limit && !c.removeOldestCompleted() {
			c.mu.Unlock()
			result, out, err := execute()
			return result, out, err, false
		}
		entry := &requestIDEntry{done: make(chan struct{})}
		c.entries[key] = entry
		c.mu.Unlock()

		result, out, err := execute()
		if err != nil {
			data, _ := encodeCallToolResult(result)
			c.mu.Lock()
			entry.result = data
			entry.err = err
			delete(c.entries, key)
			close(entry.done)
			c.mu.Unlock()
			return result, out, err, false
		}
		data, copyErr := encodeCallToolResult(result)
		c.mu.Lock()
		if copyErr != nil {
			delete(c.entries, key)
			close(entry.done)
			c.mu.Unlock()
			return result, out, nil, false
		}
		entry.result = data
		entry.completedAt = time.Now()
		close(entry.done)
		c.mu.Unlock()
		copy, copyErr := decodeCallToolResult(data)
		if copyErr != nil {
			return result, out, nil, false
		}
		return copy, out, nil, false
	}
}

func (c *requestIDCache) removeExpired(now time.Time) {
	for key, entry := range c.entries {
		if !entry.completedAt.IsZero() && now.Sub(entry.completedAt) >= c.ttl {
			delete(c.entries, key)
		}
	}
}

func (c *requestIDCache) removeOldestCompleted() bool {
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.entries {
		if entry.completedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || entry.completedAt.Before(oldest) {
			oldestKey, oldest = key, entry.completedAt
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(c.entries, oldestKey)
	return true
}

func idempotencyKey(userID uint64, requestID, tool string, input any) (string, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	var canonical any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return "", err
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return stringKey(userID, requestID, tool, hex.EncodeToString(hash[:])), nil
}

func stringKey(userID uint64, requestID, tool, hash string) string {
	return strconv.FormatUint(userID, 10) + "|" + requestID + "|" + tool + "|" + hash
}

func encodeCallToolResult(result *mcp.CallToolResult) ([]byte, error) {
	return json.Marshal(result)
}

func decodeCallToolResult(data []byte) (*mcp.CallToolResult, error) {
	if string(data) == "null" {
		return nil, nil
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
