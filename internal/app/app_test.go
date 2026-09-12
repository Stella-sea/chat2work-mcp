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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
