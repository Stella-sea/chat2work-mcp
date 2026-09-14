package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordingOffice struct{ calls [][]string }

func (o *recordingOffice) Execute(_ context.Context, root string, args []string) (string, error) {
	o.calls = append(o.calls, append([]string(nil), args...))
	if len(args) >= 2 && args[0] == "create" {
		path := filepath.Join(root, filepath.FromSlash(args[1]))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte("fake document"), 0o640); err != nil {
			return "", err
		}
	}
	return "ok", nil
}

func signedContext(t *testing.T, secret string, userID uint64, expiresAt int64) string {
	t.Helper()
	payload, err := json.Marshal(userContext{UserID: userID, ExpiresAt: expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return "v1." + encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestDeeixResolver(t *testing.T) {
	now := time.Unix(100, 0)
	resolver := DeeixResolver{Secret: "secret", Now: func() time.Time { return now }}
	headers := make(http.Header)
	headers.Set(userContextHeader, signedContext(t, "secret", 42, 101))
	identity, err := resolver.ResolveIdentity(headers)
	if err != nil || identity.UserID != 42 {
		t.Fatalf("ResolveIdentity() = %+v, %v", identity, err)
	}
	headers.Set(userContextHeader, signedContext(t, "other", 42, 101))
	if _, err := resolver.ResolveIdentity(headers); err == nil {
		t.Fatal("accepted a forged signature")
	}
	headers.Set(userContextHeader, signedContext(t, "secret", 42, 100))
	if _, err := resolver.ResolveIdentity(headers); err == nil {
		t.Fatal("accepted an expired token")
	}
}

func TestFilesIsolateTenantsAndRejectEscapes(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := Files{workspace: workspace, maxBytes: 1024}
	if err := files.Write("alice", "notes/todo.txt", "private", false); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Read("bob", "notes/todo.txt", 0); err == nil {
		t.Fatal("tenant read another tenant's file")
	}
	for _, path := range []string{"../escape.txt", "/absolute.txt"} {
		if err := files.Write("alice", path, "bad", false); err == nil {
			t.Fatalf("accepted unsafe path %q", path)
		}
	}
	data, err := files.Read("alice", "notes/todo.txt", 0)
	if err != nil || string(data) != "private" {
		t.Fatalf("Read() = %q, %v", data, err)
	}
}

func TestFilesRejectSymlinkEscapes(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := workspace.Root("alice")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	files := Files{workspace: workspace, maxBytes: 1024}
	if _, err := files.Read("alice", "link.txt", 0); err == nil {
		t.Fatal("read through an escaping symlink")
	}
	if err := files.Write("alice", "link.txt", "replacement", true); err == nil {
		t.Fatal("wrote through an escaping symlink")
	}
}

func TestFilesEditRequiresOneMatch(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := Files{workspace: workspace, maxBytes: 1024}
	if err := files.Write("alice", "test.txt", "same same", false); err != nil {
		t.Fatal(err)
	}
	if err := files.Edit("alice", "test.txt", "same", "new"); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("Edit() error = %v, want multiple-match error", err)
	}
	if err := files.Edit("alice", "test.txt", "missing", "new"); err == nil {
		t.Fatal("Edit accepted a missing match")
	}
}

func TestDownloadSignerRejectsTamperAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	signer := NewDownloadSigner("secret", "https://files.example.com", time.Minute)
	signer.now = func() time.Time { return now }
	link, expires, err := signer.URL("user-42", "reports/q1.docx")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if err := signer.verify("user-42", "reports/q1.docx", query.Get("exp"), query.Get("sig")); err != nil {
		t.Fatalf("valid link rejected: %v", err)
	}
	if err := signer.verify("user-42", "reports/other.docx", query.Get("exp"), query.Get("sig")); err == nil {
		t.Fatal("accepted a tampered path")
	}
	if !expires.After(now) {
		t.Fatal("link should expire in the future")
	}
	expired := NewDownloadSigner("secret", "", time.Minute)
	expired.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := expired.verify("user-42", "reports/q1.docx", query.Get("exp"), query.Get("sig")); err == nil {
		t.Fatal("accepted an expired link")
	}
	if err := NewDownloadSigner("other", "", time.Minute).verify("user-42", "reports/q1.docx", query.Get("exp"), query.Get("sig")); err == nil {
		t.Fatal("accepted a link signed with a different secret")
	}
}

func TestDownloadHandlerServesSignedFile(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := Files{workspace: workspace, maxBytes: 1024}
	if err := files.Write("user-42", "notes.txt", "hello download", false); err != nil {
		t.Fatal(err)
	}
	app := &App{config: Config{}, access: NewAccessManager(Config{}), files: files, downloads: NewDownloadSigner("secret", "", time.Minute)}
	link, _, err := app.downloads.URL("user-42", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, link, nil)
	recorder := httptest.NewRecorder()
	app.DownloadHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body, _ := io.ReadAll(recorder.Result().Body)
	if string(body) != "hello download" {
		t.Fatalf("body = %q", body)
	}
	bad := httptest.NewRequest(http.MethodGet, "/download?w=user-42&p=notes.txt&exp=1&sig=x", nil)
	badRecorder := httptest.NewRecorder()
	app.DownloadHandler().ServeHTTP(badRecorder, bad)
	if badRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", badRecorder.Code)
	}
}

func TestAuditorRecordsStructuredEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{config: Config{StdioTenantID: "42"}, access: NewAccessManager(Config{}), audit: auditor}
	handler := instrument(app, "fs_read", func(context.Context, *mcp.CallToolRequest, readInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom"}}}, nil, nil
	})
	if _, _, err := handler(context.Background(), &mcp.CallToolRequest{}, readInput{Workspace: "user-42", Path: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := auditor.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event AuditEvent
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if event.Tool != "fs_read" || event.UserID != 42 || event.Outcome != "error" || event.Error != "boom" || event.Workspace != "user-42" || event.Path != "a.txt" {
		t.Fatalf("unexpected audit event: %+v", event)
	}
}

func TestWorkspaceQuotaEnforced(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	byteQuota := Files{workspace: workspace, maxBytes: 1024, maxWorkspaceBytes: 10}
	if err := byteQuota.Write("alice", "a.txt", "123456", false); err != nil {
		t.Fatal(err)
	}
	if err := byteQuota.Write("alice", "b.txt", "123456", false); err == nil {
		t.Fatal("expected storage quota error")
	}
	countWorkspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fileQuota := Files{workspace: countWorkspace, maxBytes: 1024, maxWorkspaceFiles: 2}
	if err := fileQuota.Write("alice", "a.txt", "1", false); err != nil {
		t.Fatal(err)
	}
	if err := fileQuota.Write("alice", "b.txt", "2", false); err != nil {
		t.Fatal(err)
	}
	if err := fileQuota.Write("alice", "c.txt", "3", false); err == nil {
		t.Fatal("expected file count quota error")
	}
	if err := fileQuota.Write("alice", "a.txt", "1x", true); err != nil {
		t.Fatalf("overwrite should bypass the new-file count check: %v", err)
	}
}

func TestListAndSearchTruncate(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := Files{workspace: workspace, maxBytes: 1024}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := files.Write("alice", name, "x", false); err != nil {
			t.Fatal(err)
		}
	}
	items, truncated, err := files.List("alice", ".", false, 2)
	if err != nil || !truncated || len(items) != 2 {
		t.Fatalf("List() = %v, truncated=%v, err=%v", items, truncated, err)
	}
	matches, truncated, err := files.Search("alice", "*.txt", "", ".", 1)
	if err != nil || !truncated || len(matches) != 1 {
		t.Fatalf("Search() = %v, truncated=%v, err=%v", matches, truncated, err)
	}
}

func TestAuditorRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewAuditor(path, 200, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		auditor.Record(AuditEvent{Tool: "fs_read", Path: "a/long/path/that/makes/the/record/larger.txt"})
	}
	if err := auditor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated backup missing: %v", err)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("rotation kept more backups than maxBackups")
	}
}

func TestCheckOfficeInput(t *testing.T) {
	dir := t.TempDir()
	oversized := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(oversized, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkOfficeInput(oversized, 50, 0); err == nil {
		t.Fatal("expected size limit error")
	}
	bomb := filepath.Join(dir, "bomb.docx")
	file, err := os.Create(bomb)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(make([]byte, 10000)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkOfficeInput(bomb, 1<<20, 5000); err == nil {
		t.Fatal("expected uncompressed size limit error")
	}
}

type failingBatchOffice struct{ calls [][]string }

func (o *failingBatchOffice) Execute(_ context.Context, _ string, args []string) (string, error) {
	o.calls = append(o.calls, append([]string(nil), args...))
	return "", errors.New("boom")
}

func TestDocEditKeepsOriginalOnFailure(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := Files{workspace: workspace, maxBytes: 1 << 20}
	if err := files.Write("user-42", "doc.docx", "original", false); err != nil {
		t.Fatal(err)
	}
	app := &App{config: Config{OfficeTimeout: "1s", StdioTenantID: "42", MaxUncompressedBytes: 1 << 20}, access: NewAccessManager(Config{}), files: files, officeEngine: &failingBatchOffice{}}
	result, _, _ := app.docEdit(context.Background(), &mcp.CallToolRequest{}, docEditInput{Path: "doc.docx", Ops: json.RawMessage(`[{"command":"add"}]`)})
	if result == nil || !result.IsError {
		t.Fatalf("expected an error result, got %+v", result)
	}
	data, err := files.Read("user-42", "doc.docx", 0)
	if err != nil || string(data) != "original" {
		t.Fatalf("original changed on failed edit: %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Join(workspace.base, "user-42"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".chat2work-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

type headerTransport struct{ bearer, context string }

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.bearer)
	clone.Header.Set("X-Deeix-User-Context", t.context)
	return http.DefaultTransport.RoundTrip(clone)
}

func TestHTTPHandlerEndToEnd(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := Files{workspace: workspace, maxBytes: 1 << 20}
	app := &App{
		config:   Config{MCPToken: "tok", MaxListResults: 100, OfficeTimeout: "1s"},
		resolver: DeeixResolver{Secret: "secret"},
		access:   NewAccessManager(Config{}),
		files:    files,
		audit:    &Auditor{},
		logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	server := httptest.NewServer(app.HTTPHandler())
	defer server.Close()

	signedToken := signedContext(t, "secret", 42, time.Now().Add(time.Minute).Unix())
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             server.URL,
		HTTPClient:           &http.Client{Transport: headerTransport{bearer: "tok", context: signedToken}},
		DisableStandaloneSSE: true,
	}
	ctx := context.Background()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fs_write", Arguments: map[string]any{"path": "hello.txt", "content": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool error: %+v", result)
	}
	data, err := files.Read("user-42", "hello.txt", 0)
	if err != nil || string(data) != "hi" {
		t.Fatalf("Read() = %q, %v", data, err)
	}
}

func TestStatusRecorderPreservesFlush(t *testing.T) {
	recorder := httptest.NewRecorder()
	wrapped := &statusRecorder{ResponseWriter: recorder, status: http.StatusOK}
	if _, err := wrapped.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := http.NewResponseController(wrapped).Flush(); err != nil {
		t.Fatalf("flush through wrapper failed: %v", err)
	}
	if !recorder.Flushed {
		t.Fatal("underlying writer was not flushed through the wrapper")
	}
}

func TestRequestLoggingIncludesRequestID(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	app := &App{config: Config{MCPToken: "tok", WorkspaceRoot: t.TempDir(), OfficeCLIPath: "missing"}, access: NewAccessManager(Config{}), audit: &Auditor{}, logger: logger}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("X-Request-Id", "req-abc")
	app.HTTPHandler().ServeHTTP(httptest.NewRecorder(), request)
	output := buffer.String()
	if !strings.Contains(output, `"request_id":"req-abc"`) || !strings.Contains(output, "http_request") {
		t.Fatalf("expected request id in logs, got: %s", output)
	}
}

func TestToolCallLoggingIncludesRequestID(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	app := &App{config: Config{StdioTenantID: "42"}, access: NewAccessManager(Config{}), audit: &Auditor{}, logger: logger}
	handler := instrument(app, "fs_read", func(context.Context, *mcp.CallToolRequest, readInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	ctx := withRequestID(context.Background(), "req-xyz")
	if _, _, err := handler(ctx, &mcp.CallToolRequest{}, readInput{Path: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	output := buffer.String()
	if !strings.Contains(output, `"request_id":"req-xyz"`) || !strings.Contains(output, "tool_call") {
		t.Fatalf("expected tool_call log with request id, got: %s", output)
	}
}

func TestInstrumentDeduplicatesMutatingCalls(t *testing.T) {
	app := idempotencyTestApp()
	var calls atomic.Int32
	handler := instrument(app, "fs_write", func(_ context.Context, _ *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Content}}}, nil, nil
	})
	req := idempotencyRequest(t, 42, "request-1")
	first, _, err := handler(context.Background(), req, writeInput{Path: "a.txt", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	first.Content[0].(*mcp.TextContent).Text = "changed"
	second, _, err := handler(context.Background(), req, writeInput{Path: "a.txt", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || firstText(second) != "first" {
		t.Fatalf("calls=%d, second=%+v", calls.Load(), second)
	}
}

func TestInstrumentSeparatesRequestIDByParametersAndUser(t *testing.T) {
	app := idempotencyTestApp()
	var calls atomic.Int32
	handler := instrument(app, "fs_write", func(context.Context, *mcp.CallToolRequest, writeInput) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil, nil
	})
	if _, _, err := handler(context.Background(), idempotencyRequest(t, 42, "request-1"), writeInput{Path: "a.txt", Content: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handler(context.Background(), idempotencyRequest(t, 42, "request-1"), writeInput{Path: "a.txt", Content: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handler(context.Background(), idempotencyRequest(t, 73, "request-1"), writeInput{Path: "a.txt", Content: "one"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want 3", calls.Load())
	}
}

func TestInstrumentDeduplicatesConcurrentCalls(t *testing.T) {
	app := idempotencyTestApp()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	handler := instrument(app, "fs_write", func(context.Context, *mcp.CallToolRequest, writeInput) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		close(started)
		<-release
		return &mcp.CallToolResult{}, nil, nil
	})
	req := idempotencyRequest(t, 42, "request-1")
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			if _, _, err := handler(context.Background(), req, writeInput{Path: "a.txt", Content: "one"}); err != nil {
				t.Errorf("handler: %v", err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
}

func TestRequestIDCacheBypassesWhenAllEntriesAreInFlight(t *testing.T) {
	cache := newRequestIDCache(time.Minute, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var aWG sync.WaitGroup
	aWG.Add(1)
	go func() {
		defer aWG.Done()
		_, _, _, _ = cache.call("A", func() (*mcp.CallToolResult, any, error) {
			close(started)
			<-release
			return &mcp.CallToolResult{}, nil, nil
		})
	}()
	<-started

	var bCalls atomic.Int32
	b := func() (*mcp.CallToolResult, any, error) {
		bCalls.Add(1)
		return &mcp.CallToolResult{}, nil, nil
	}
	if _, _, err, deduplicated := cache.call("B", b); err != nil || deduplicated {
		t.Fatalf("first B call: err=%v, deduplicated=%v", err, deduplicated)
	}
	cache.mu.Lock()
	entries := len(cache.entries)
	cache.mu.Unlock()
	if entries > 1 {
		t.Fatalf("cache entries=%d, want at most 1", entries)
	}

	close(release)
	aWG.Wait()
	if _, _, err, deduplicated := cache.call("B", b); err != nil || deduplicated {
		t.Fatalf("second B call: err=%v, deduplicated=%v", err, deduplicated)
	}
	if bCalls.Load() != 2 {
		t.Fatalf("B calls=%d, want 2", bCalls.Load())
	}
}

func TestInstrumentDoesNotDeduplicateWithoutSignedRequestID(t *testing.T) {
	app := idempotencyTestApp()
	var calls atomic.Int32
	handler := instrument(app, "fs_write", func(context.Context, *mcp.CallToolRequest, writeInput) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil, nil
	})
	for range 2 {
		if _, _, err := handler(context.Background(), idempotencyRequest(t, 42, ""), writeInput{Path: "a.txt", Content: "one"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
}

func TestInstrumentReplaysMCPErrorButNotGoError(t *testing.T) {
	app := idempotencyTestApp()
	var resultCalls, errorCalls int
	resultHandler := instrument(app, "fs_write", func(context.Context, *mcp.CallToolRequest, writeInput) (*mcp.CallToolResult, any, error) {
		resultCalls++
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "tool failure"}}}, nil, nil
	})
	errorHandler := instrument(app, "fs_edit", func(context.Context, *mcp.CallToolRequest, editInput) (*mcp.CallToolResult, any, error) {
		errorCalls++
		return nil, nil, errors.New("transport failure")
	})
	req := idempotencyRequest(t, 42, "request-1")
	for range 2 {
		result, _, err := resultHandler(context.Background(), req, writeInput{Path: "a.txt", Content: "one"})
		if err != nil || result == nil || !result.IsError || firstText(result) != "tool failure" {
			t.Fatalf("result=%+v, err=%v", result, err)
		}
	}
	for range 2 {
		if _, _, err := errorHandler(context.Background(), req, editInput{Path: "a.txt", OldText: "a", NewText: "b"}); err == nil {
			t.Fatal("expected Go error")
		}
	}
	if resultCalls != 1 || errorCalls != 2 {
		t.Fatalf("result calls=%d, error calls=%d", resultCalls, errorCalls)
	}
}

func idempotencyTestApp() *App {
	return &App{
		config:     Config{MCPUserContextSecret: "secret"},
		resolver:   DeeixResolver{Secret: "secret"},
		audit:      &Auditor{},
		logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		requestIDs: newRequestIDCache(time.Minute, 100),
	}
}

func idempotencyRequest(t *testing.T, userID uint64, requestID string) *mcp.CallToolRequest {
	t.Helper()
	payload, err := json.Marshal(userContext{UserID: userID, RequestID: requestID, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(encoded))
	headers := make(http.Header)
	headers.Set(userContextHeader, "v1."+encoded+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: headers}}
}

func TestHealthAndReadinessEndpoints(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	app := &App{config: Config{WorkspaceRoot: t.TempDir(), OfficeCLIPath: self}, access: NewAccessManager(Config{}), audit: &Auditor{}}
	handler := app.HTTPHandler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", recorder.Code)
	}
	app.config.OfficeCLIPath = "chat2work-definitely-missing-binary"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with missing officecli = %d, want 503", recorder.Code)
	}
}

func TestDocumentToolsUseVerifiedOfficeCLIArguments(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	office := &recordingOffice{}
	app := &App{config: Config{OfficeTimeout: "1s", StdioTenantID: "42"}, access: NewAccessManager(Config{}), files: Files{workspace: workspace, maxBytes: 1024}, officeEngine: office}
	req := &mcp.CallToolRequest{}
	if result, _, err := app.docCreate(context.Background(), req, docCreateInput{Path: "report.docx", Kind: "docx", Ops: json.RawMessage(`[{"command":"add"}]`)}); err != nil || result.IsError {
		t.Fatalf("docCreate() = %+v, %v", result, err)
	}
	if len(office.calls) != 2 ||
		office.calls[0][0] != "create" ||
		!strings.HasPrefix(office.calls[0][1], "report.chat2work-") ||
		!strings.HasSuffix(office.calls[0][1], ".docx") ||
		office.calls[1][0] != "batch" ||
		office.calls[1][1] != office.calls[0][1] ||
		office.calls[1][2] != "--commands" {
		t.Fatalf("unexpected office calls: %#v", office.calls)
	}
	if result, _, err := app.docCreate(context.Background(), req, docCreateInput{Path: "report.pdf"}); err != nil || !result.IsError {
		t.Fatalf("expected invalid extension result, got %+v, %v", result, err)
	}
}
