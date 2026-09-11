package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type App struct {
	config       Config
	resolver     TenantResolver
	access       *AccessManager
	files        Files
	officeEngine OfficeEngine
	downloads    DownloadSigner
	audit        *Auditor
}

func New(config Config) (*App, error) {
	workspace, err := NewWorkspace(config.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	auditor, err := NewAuditor(config.AuditLogPath)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &App{
		config:   config,
		resolver: DeeixResolver{Secret: config.MCPUserContextSecret},
		access:   NewAccessManager(config),
		files: Files{
			workspace:         workspace,
			maxBytes:          config.MaxFileBytes,
			maxWorkspaceBytes: config.MaxWorkspaceBytes,
			maxWorkspaceFiles: config.MaxWorkspaceFiles,
			deleteEnabled:     config.DeleteEnabled,
		},
		officeEngine: NewOfficeCLI(config.OfficeCLIPath, config.MaxOfficeConcurrency),
		downloads:    NewDownloadSigner(config.DownloadSecret, config.DownloadBaseURL, config.DownloadDuration()),
		audit:        auditor,
	}, nil
}

// Close releases the audit log file.
func (a *App) Close() error { return a.audit.Close() }

func (a *App) Server() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "chat2work-mcp", Version: "0.1.0"}, nil)
	const workspaceNote = " Omit 'workspace' to use your private workspace; pass an administrator-assigned workspace id to operate on a shared one."
	mcp.AddTool(server, &mcp.Tool{Name: "fs_list", Description: "List files in a workspace." + workspaceNote}, instrument(a, "fs_list", a.fsList))
	mcp.AddTool(server, &mcp.Tool{Name: "workspace_list", Description: "List the workspaces assigned to you by the administrator."}, instrument(a, "workspace_list", a.workspaceList))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_read", Description: "Read a file from a workspace." + workspaceNote}, instrument(a, "fs_read", a.fsRead))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_write", Description: "Atomically create or replace a file in a workspace." + workspaceNote}, instrument(a, "fs_write", a.fsWrite))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_edit", Description: "Replace exactly one text occurrence in a workspace file." + workspaceNote}, instrument(a, "fs_edit", a.fsEdit))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_move", Description: "Move a file within a workspace." + workspaceNote}, instrument(a, "fs_move", a.fsMove))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_delete", Description: "Delete one file from a workspace when enabled by the administrator." + workspaceNote}, instrument(a, "fs_delete", a.fsDelete))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_search", Description: "Find files by glob pattern in a workspace." + workspaceNote}, instrument(a, "fs_search", a.fsSearch))
	mcp.AddTool(server, &mcp.Tool{Name: "fs_link", Description: "Get a time-limited download link for a workspace file, so the user can save it." + workspaceNote}, instrument(a, "fs_link", a.fsLink))
	mcp.AddTool(server, &mcp.Tool{Name: "doc_create", Description: "Create an Office document in a workspace using OfficeCLI." + workspaceNote}, instrument(a, "doc_create", a.docCreate))
	mcp.AddTool(server, &mcp.Tool{Name: "doc_edit", Description: "Apply OfficeCLI operations to a document in a workspace." + workspaceNote}, instrument(a, "doc_edit", a.docEdit))
	mcp.AddTool(server, &mcp.Tool{Name: "doc_query", Description: "Query an Office document in a workspace using OfficeCLI JSON output." + workspaceNote}, instrument(a, "doc_query", a.docQuery))
	return server
}

func (a *App) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", requireBearer(a.config.MCPToken, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return a.Server() }, &mcp.StreamableHTTPOptions{Stateless: true})))
	mux.Handle("/download", a.DownloadHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", a.readyz)
	return mux
}

// readyz reports whether the service can serve tool calls: OfficeCLI must be
// resolvable and the workspace root must be writable.
func (a *App) readyz(w http.ResponseWriter, _ *http.Request) {
	if _, err := exec.LookPath(a.config.OfficeCLIPath); err != nil {
		http.Error(w, "officecli not found", http.StatusServiceUnavailable)
		return
	}
	if err := os.MkdirAll(a.config.WorkspaceRoot, 0o750); err != nil {
		http.Error(w, "workspace root not writable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (a *App) RunStdio(ctx context.Context) error {
	if a.config.StdioTenantID == "" {
		return fmt.Errorf("stdio_tenant_id is required for stdio mode")
	}
	return a.Server().Run(ctx, &mcp.StdioTransport{})
}

func (a *App) identity(req *mcp.CallToolRequest) (Identity, error) {
	if req.Extra != nil && req.Extra.Header != nil {
		return identityFromRequest(a.resolver, req.Extra.Header)
	}
	if a.config.StdioTenantID != "" {
		var id uint64
		if _, err := fmt.Sscan(a.config.StdioTenantID, &id); err != nil || id == 0 {
			return Identity{}, fmt.Errorf("stdio_tenant_id must be a numeric user id")
		}
		return Identity{UserID: id}, nil
	}
	return Identity{}, fmt.Errorf("authorization failed: signed user context is required")
}

func (a *App) grant(req *mcp.CallToolRequest, workspace string, write, remove bool) (WorkspaceGrant, error) {
	identity, err := a.identity(req)
	if err != nil {
		return WorkspaceGrant{}, err
	}
	grant, err := a.access.Grant(identity.UserID, workspace)
	if err != nil {
		return WorkspaceGrant{}, err
	}
	if remove && !canDelete(grant.Access) {
		return WorkspaceGrant{}, fmt.Errorf("workspace %q requires owner access for deletion", grant.ID)
	}
	if write && !canWrite(grant.Access) {
		return WorkspaceGrant{}, fmt.Errorf("workspace %q is read-only", grant.ID)
	}
	return grant, nil
}

func textResult(value any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
}
func toolError(err error) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
}

type pathInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path" jsonschema:"relative workspace path"`
}
type listInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path" jsonschema:"relative workspace path"`
	Recursive bool   `json:"recursive,omitempty"`
}
type readInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path"`
	MaxBytes  int64  `json:"max_bytes,omitempty"`
}
type writeInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite,omitempty"`
}
type editInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path"`
	OldText   string `json:"old_text"`
	NewText   string `json:"new_text"`
}
type moveInput struct {
	Workspace   string `json:"workspace,omitempty"`
	Source      string `json:"src"`
	Destination string `json:"dst"`
}
type deleteInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path"`
	Confirm   bool   `json:"confirm"`
}
type searchInput struct {
	Workspace string `json:"workspace,omitempty"`
	Pattern   string `json:"pattern"`
	Query     string `json:"query,omitempty"`
	Path      string `json:"path,omitempty"`
}
type linkInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path"`
}

func (a *App) workspaceList(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	identity, err := a.identity(req)
	if err != nil {
		return toolError(err)
	}
	return textResult(a.access.List(identity.UserID))
}
func (a *App) fsList(_ context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, false, false)
	if err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.RLock()
	defer lock.RUnlock()
	items, truncated, err := a.files.List(grant.ID, in.Path, in.Recursive, a.config.MaxListResults)
	if err != nil {
		return toolError(err)
	}
	return textResult(map[string]any{"items": items, "truncated": truncated, "limit": a.config.MaxListResults})
}
func (a *App) fsRead(_ context.Context, req *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, false, false)
	if err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.RLock()
	defer lock.RUnlock()
	content, err := a.files.Read(grant.ID, in.Path, in.MaxBytes)
	if err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"content": string(content)})
}
func (a *App) fsWrite(_ context.Context, req *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, true, false)
	if err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := a.files.Write(grant.ID, in.Path, in.Content, in.Overwrite); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "written"})
}
func (a *App) fsEdit(_ context.Context, req *mcp.CallToolRequest, in editInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, true, false)
	if err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := a.files.Edit(grant.ID, in.Path, in.OldText, in.NewText); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "edited"})
}
func (a *App) fsMove(_ context.Context, req *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, true, false)
	if err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := a.files.Move(grant.ID, in.Source, in.Destination); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "moved"})
}
func (a *App) fsDelete(_ context.Context, req *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, true, true)
	if err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := a.files.Delete(grant.ID, in.Path, in.Confirm); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "deleted"})
}
func (a *App) fsSearch(_ context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, false, false)
	if err != nil {
		return toolError(err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return toolError(fmt.Errorf("pattern is required"))
	}
	lock := a.access.Lock(grant.ID)
	lock.RLock()
	defer lock.RUnlock()
	matches, truncated, err := a.files.Search(grant.ID, in.Pattern, in.Query, in.Path, a.config.MaxSearchResults)
	if err != nil {
		return toolError(err)
	}
	return textResult(map[string]any{"matches": matches, "truncated": truncated, "limit": a.config.MaxSearchResults})
}
func (a *App) fsLink(_ context.Context, req *mcp.CallToolRequest, in linkInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, false, false)
	if err != nil {
		return toolError(err)
	}
	if _, err := a.documentPath(grant.ID, in.Path, false); err != nil {
		return toolError(err)
	}
	cleanPath := filepath.ToSlash(filepath.Clean(in.Path))
	link, expires, err := a.downloads.URL(grant.ID, cleanPath)
	if err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{
		"workspace":    grant.ID,
		"path":         cleanPath,
		"download_url": link,
		"expires_at":   expires.UTC().Format(time.RFC3339),
		"note":         "Present this link to the user. It expires and grants read access to this single file only.",
	})
}

type docCreateInput struct {
	Workspace string          `json:"workspace,omitempty"`
	Path      string          `json:"path"`
	Kind      string          `json:"kind"`
	Overwrite bool            `json:"overwrite,omitempty"`
	Ops       json.RawMessage `json:"ops,omitempty"`
}
type docEditInput struct {
	Workspace string          `json:"workspace,omitempty"`
	Path      string          `json:"path"`
	Ops       json.RawMessage `json:"ops"`
}
type docQueryInput struct {
	Workspace string `json:"workspace,omitempty"`
	Path      string `json:"path"`
	Selector  string `json:"selector"`
}

func (a *App) runOffice(ctx context.Context, workspace string, args ...string) (*mcp.CallToolResult, any, error) {
	root, err := a.files.workspace.Root(workspace)
	if err != nil {
		return toolError(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, a.config.OfficeDuration())
	defer cancel()
	output, err := a.officeEngine.Execute(callCtx, root, args)
	if err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"output": output})
}
func (a *App) docCreate(ctx context.Context, req *mcp.CallToolRequest, in docCreateInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, true, false)
	if err != nil {
		return toolError(err)
	}
	resolved, err := a.documentPath(grant.ID, in.Path, true)
	if err != nil {
		return toolError(err)
	}
	if err := validateDocumentPath(in.Path, in.Kind); err != nil {
		return toolError(err)
	}
	isNew := true
	if _, statErr := os.Stat(resolved); statErr == nil {
		isNew = false
	}
	if err := a.files.CheckWriteAllowed(grant.ID, isNew); err != nil {
		return toolError(err)
	}
	if len(in.Ops) > 0 && string(in.Ops) != "null" && !isJSONArray(in.Ops) {
		return toolError(fmt.Errorf("ops must be a JSON array"))
	}
	args := []string{"create", in.Path}
	if in.Overwrite {
		args = append(args, "--force")
	}
	lock := a.access.Lock(grant.ID)
	lock.Lock()
	defer lock.Unlock()
	result, _, err := a.runOffice(ctx, grant.ID, args...)
	if err != nil || len(in.Ops) == 0 || string(in.Ops) == "null" {
		return result, nil, err
	}
	return a.runOffice(ctx, grant.ID, "batch", in.Path, "--commands", string(in.Ops))
}
func (a *App) docEdit(ctx context.Context, req *mcp.CallToolRequest, in docEditInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, true, false)
	if err != nil {
		return toolError(err)
	}
	if !isJSONArray(in.Ops) {
		return toolError(fmt.Errorf("ops must be a JSON array"))
	}
	if _, err := a.documentPath(grant.ID, in.Path, false); err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.Lock()
	defer lock.Unlock()
	return a.runOffice(ctx, grant.ID, "batch", in.Path, "--commands", string(in.Ops))
}

func isJSONArray(raw json.RawMessage) bool {
	return json.Valid(raw) && len(raw) > 0 && strings.HasPrefix(strings.TrimSpace(string(raw)), "[")
}

func validateDocumentPath(path, kind string) error {
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	kind = strings.ToLower(strings.TrimSpace(kind))
	if extension != "docx" && extension != "xlsx" && extension != "pptx" {
		return fmt.Errorf("document path must end in .docx, .xlsx, or .pptx")
	}
	if kind != "" && kind != extension {
		return fmt.Errorf("kind %q does not match path extension .%s", kind, extension)
	}
	return nil
}
func (a *App) docQuery(ctx context.Context, req *mcp.CallToolRequest, in docQueryInput) (*mcp.CallToolResult, any, error) {
	grant, err := a.grant(req, in.Workspace, false, false)
	if err != nil {
		return toolError(err)
	}
	if _, err := a.documentPath(grant.ID, in.Path, false); err != nil {
		return toolError(err)
	}
	lock := a.access.Lock(grant.ID)
	lock.RLock()
	defer lock.RUnlock()
	return a.runOffice(ctx, grant.ID, "query", in.Path, in.Selector, "--json")
}

func (a *App) documentPath(tenant, path string, allowMissing bool) (string, error) {
	root, err := a.files.workspace.Root(tenant)
	if err != nil {
		return "", err
	}
	resolved, err := resolve(root, path, allowMissing)
	return resolved, friendlyPathError(err)
}
