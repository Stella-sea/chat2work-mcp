package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordingOffice struct{ calls [][]string }

func (o *recordingOffice) Execute(_ context.Context, _ string, args []string) (string, error) {
	o.calls = append(o.calls, append([]string(nil), args...))
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
	tenant, err := resolver.ResolveTenant(headers)
	if err != nil || tenant != "42" {
		t.Fatalf("ResolveTenant() = %q, %v", tenant, err)
	}
	headers.Set(userContextHeader, signedContext(t, "other", 42, 101))
	if _, err := resolver.ResolveTenant(headers); err == nil {
		t.Fatal("accepted a forged signature")
	}
	headers.Set(userContextHeader, signedContext(t, "secret", 42, 100))
	if _, err := resolver.ResolveTenant(headers); err == nil {
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

func TestDocumentToolsUseVerifiedOfficeCLIArguments(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	office := &recordingOffice{}
	app := &App{config: Config{OfficeTimeout: "1s", StdioTenantID: "alice"}, files: Files{workspace: workspace, maxBytes: 1024}, officeEngine: office}
	req := &mcp.CallToolRequest{}
	if result, _, err := app.docCreate(context.Background(), req, docCreateInput{Path: "report.docx", Kind: "docx", Ops: json.RawMessage(`[{"command":"add"}]`)}); err != nil || result.IsError {
		t.Fatalf("docCreate() = %+v, %v", result, err)
	}
	if len(office.calls) != 2 || strings.Join(office.calls[0], " ") != "create report.docx" || office.calls[1][0] != "batch" || office.calls[1][2] != "--commands" {
		t.Fatalf("unexpected office calls: %#v", office.calls)
	}
	if result, _, err := app.docCreate(context.Background(), req, docCreateInput{Path: "report.pdf"}); err != nil || !result.IsError {
		t.Fatalf("expected invalid extension result, got %+v, %v", result, err)
	}
}
