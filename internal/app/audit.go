package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AuditEvent is one structured tool-call record. It never contains file
// content or secrets.
type AuditEvent struct {
	Time        time.Time `json:"time"`
	UserID      uint64    `json:"user_id,omitempty"`
	Workspace   string    `json:"workspace,omitempty"`
	Tool        string    `json:"tool"`
	Path        string    `json:"path,omitempty"`
	Outcome     string    `json:"outcome"`
	Error       string    `json:"error,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	ResultBytes int       `json:"result_bytes,omitempty"`
}

// Auditor appends JSON Lines audit records with size-based rotation. A nil or
// disabled auditor is a no-op so audit stays optional.
type Auditor struct {
	mu         sync.Mutex
	file       *os.File
	enabled    bool
	path       string
	size       int64
	maxBytes   int64
	maxBackups int
}

func NewAuditor(path string, maxBytes int64, maxBackups int) (*Auditor, error) {
	if path == "" {
		return &Auditor{}, nil
	}
	auditor := &Auditor{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := auditor.open(); err != nil {
		return nil, err
	}
	auditor.enabled = true
	return auditor, nil
}

func (a *Auditor) open() error {
	file, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	a.file = file
	a.size = info.Size()
	return nil
}

func (a *Auditor) Record(event AuditEvent) {
	if a == nil || !a.enabled || a.file == nil {
		return
	}
	event.Time = time.Now().UTC()
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.maxBytes > 0 && a.size+int64(len(data)) > a.maxBytes {
		a.rotate()
	}
	if a.file == nil {
		return
	}
	written, _ := a.file.Write(data)
	a.size += int64(written)
}

// rotate must be called with a.mu held.
func (a *Auditor) rotate() {
	_ = a.file.Close()
	a.file = nil
	if a.maxBackups > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", a.path, a.maxBackups))
		for i := a.maxBackups - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1))
		}
		_ = os.Rename(a.path, a.path+".1")
	} else {
		_ = os.Remove(a.path)
	}
	if err := a.open(); err != nil {
		a.enabled = false
	}
}

func (a *Auditor) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	return a.file.Close()
}

// auditable lets the instrumentation wrapper record workspace and path without
// leaking into the tool schemas.
type auditable interface{ auditTarget() (string, string) }

func (in listInput) auditTarget() (string, string)      { return in.Workspace, in.Path }
func (in readInput) auditTarget() (string, string)      { return in.Workspace, in.Path }
func (in writeInput) auditTarget() (string, string)     { return in.Workspace, in.Path }
func (in editInput) auditTarget() (string, string)      { return in.Workspace, in.Path }
func (in moveInput) auditTarget() (string, string)      { return in.Workspace, in.Source }
func (in deleteInput) auditTarget() (string, string)    { return in.Workspace, in.Path }
func (in searchInput) auditTarget() (string, string)    { return in.Workspace, in.Path }
func (in linkInput) auditTarget() (string, string)      { return in.Workspace, in.Path }
func (in docCreateInput) auditTarget() (string, string) { return in.Workspace, in.Path }
func (in docEditInput) auditTarget() (string, string)   { return in.Workspace, in.Path }
func (in docQueryInput) auditTarget() (string, string)  { return in.Workspace, in.Path }

// instrument wraps a tool handler with audit recording.
func instrument[In any](a *App, name string, h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		result, out, err := h(ctx, req, in)
		event := AuditEvent{Tool: name, Outcome: "ok", DurationMS: time.Since(start).Milliseconds()}
		if identity, idErr := a.identity(req); idErr == nil {
			event.UserID = identity.UserID
		}
		if target, ok := any(in).(auditable); ok {
			event.Workspace, event.Path = target.auditTarget()
		}
		if result != nil {
			event.ResultBytes = resultSize(result)
			if result.IsError {
				event.Outcome = "error"
				event.Error = firstText(result)
			}
		}
		if err != nil {
			event.Outcome = "error"
			event.Error = err.Error()
		}
		a.audit.Record(event)
		return result, out, err
	}
}

func resultSize(result *mcp.CallToolResult) int {
	total := 0
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			total += len(text.Text)
		}
	}
	return total
}

func firstText(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			if len(text.Text) > 300 {
				return text.Text[:300]
			}
			return text.Text
		}
	}
	return ""
}
