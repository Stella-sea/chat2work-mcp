package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type App struct {
	config       Config
	resolver     TenantResolver
	files        Files
	officeEngine OfficeEngine
}

func New(config Config) (*App, error) {
	workspace, err := NewWorkspace(config.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	return &App{config: config, resolver: DeeixResolver{Secret: config.MCPUserContextSecret}, files: Files{workspace: workspace, maxBytes: config.MaxFileBytes, deleteEnabled: config.DeleteEnabled}, officeEngine: OfficeCLI{path: config.OfficeCLIPath}}, nil
}

func (a *App) Server() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "chat2work-mcp", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_list", Description: "List files in your own workspace."}, a.fsList)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_read", Description: "Read a file from your own workspace."}, a.fsRead)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_write", Description: "Atomically create or replace a file in your own workspace."}, a.fsWrite)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_edit", Description: "Replace exactly one text occurrence in a workspace file."}, a.fsEdit)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_move", Description: "Move a file within your own workspace."}, a.fsMove)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_delete", Description: "Delete one file from your own workspace when enabled by the administrator."}, a.fsDelete)
	mcp.AddTool(server, &mcp.Tool{Name: "fs_search", Description: "Find files by glob pattern in your own workspace."}, a.fsSearch)
	mcp.AddTool(server, &mcp.Tool{Name: "doc_create", Description: "Create an Office document in your own workspace using OfficeCLI."}, a.docCreate)
	mcp.AddTool(server, &mcp.Tool{Name: "doc_edit", Description: "Apply OfficeCLI operations to a document in your own workspace."}, a.docEdit)
	mcp.AddTool(server, &mcp.Tool{Name: "doc_query", Description: "Query an Office document in your own workspace using OfficeCLI JSON output."}, a.docQuery)
	return server
}

func (a *App) HTTPHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return a.Server() }, &mcp.StreamableHTTPOptions{Stateless: true})
	return requireBearer(a.config.MCPToken, h)
}

func (a *App) RunStdio(ctx context.Context) error {
	if a.config.StdioTenantID == "" {
		return fmt.Errorf("stdio_tenant_id is required for stdio mode")
	}
	return a.Server().Run(ctx, &mcp.StdioTransport{})
}

func (a *App) tenant(req *mcp.CallToolRequest) (string, error) {
	if req.Extra != nil && req.Extra.Header != nil {
		return tenantFromRequest(a.resolver, req.Extra.Header)
	}
	if a.config.StdioTenantID != "" {
		return a.config.StdioTenantID, nil
	}
	return "", fmt.Errorf("authorization failed: signed user context is required")
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
	Path string `json:"path" jsonschema:"relative workspace path"`
}
type listInput struct {
	Path      string `json:"path" jsonschema:"relative workspace path"`
	Recursive bool   `json:"recursive,omitempty"`
}
type readInput struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}
type writeInput struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite,omitempty"`
}
type editInput struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}
type moveInput struct {
	Source      string `json:"src"`
	Destination string `json:"dst"`
}
type deleteInput struct {
	Path    string `json:"path"`
	Confirm bool   `json:"confirm"`
}
type searchInput struct {
	Pattern string `json:"pattern"`
	Query   string `json:"query,omitempty"`
	Path    string `json:"path,omitempty"`
}

func (a *App) fsList(_ context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	items, err := a.files.List(tenant, in.Path, in.Recursive)
	if err != nil {
		return toolError(err)
	}
	return textResult(items)
}
func (a *App) fsRead(_ context.Context, req *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	content, err := a.files.Read(tenant, in.Path, in.MaxBytes)
	if err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"content": string(content)})
}
func (a *App) fsWrite(_ context.Context, req *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if err := a.files.Write(tenant, in.Path, in.Content, in.Overwrite); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "written"})
}
func (a *App) fsEdit(_ context.Context, req *mcp.CallToolRequest, in editInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if err := a.files.Edit(tenant, in.Path, in.OldText, in.NewText); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "edited"})
}
func (a *App) fsMove(_ context.Context, req *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if err := a.files.Move(tenant, in.Source, in.Destination); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "moved"})
}
func (a *App) fsDelete(_ context.Context, req *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if err := a.files.Delete(tenant, in.Path, in.Confirm); err != nil {
		return toolError(err)
	}
	return textResult(map[string]string{"status": "deleted"})
}
func (a *App) fsSearch(_ context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return toolError(fmt.Errorf("pattern is required"))
	}
	matches, err := a.files.Search(tenant, in.Pattern, in.Query, in.Path)
	if err != nil {
		return toolError(err)
	}
	return textResult(matches)
}

type docCreateInput struct {
	Path      string          `json:"path"`
	Kind      string          `json:"kind"`
	Overwrite bool            `json:"overwrite,omitempty"`
	Ops       json.RawMessage `json:"ops,omitempty"`
}
type docEditInput struct {
	Path string          `json:"path"`
	Ops  json.RawMessage `json:"ops"`
}
type docQueryInput struct {
	Path     string `json:"path"`
	Selector string `json:"selector"`
}

func (a *App) runOffice(ctx context.Context, tenant string, args ...string) (*mcp.CallToolResult, any, error) {
	root, err := a.files.workspace.Root(tenant)
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
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if err := a.documentPath(tenant, in.Path, true); err != nil {
		return toolError(err)
	}
	if err := validateDocumentPath(in.Path, in.Kind); err != nil {
		return toolError(err)
	}
	if len(in.Ops) > 0 && string(in.Ops) != "null" && !isJSONArray(in.Ops) {
		return toolError(fmt.Errorf("ops must be a JSON array"))
	}
	args := []string{"create", in.Path}
	if in.Overwrite {
		args = append(args, "--force")
	}
	result, _, err := a.runOffice(ctx, tenant, args...)
	if err != nil || len(in.Ops) == 0 || string(in.Ops) == "null" {
		return result, nil, err
	}
	return a.runOffice(ctx, tenant, "batch", in.Path, "--commands", string(in.Ops))
}
func (a *App) docEdit(ctx context.Context, req *mcp.CallToolRequest, in docEditInput) (*mcp.CallToolResult, any, error) {
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if !isJSONArray(in.Ops) {
		return toolError(fmt.Errorf("ops must be a JSON array"))
	}
	if err := a.documentPath(tenant, in.Path, false); err != nil {
		return toolError(err)
	}
	return a.runOffice(ctx, tenant, "batch", in.Path, "--commands", string(in.Ops))
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
	tenant, err := a.tenant(req)
	if err != nil {
		return toolError(err)
	}
	if err := a.documentPath(tenant, in.Path, false); err != nil {
		return toolError(err)
	}
	return a.runOffice(ctx, tenant, "query", in.Path, in.Selector, "--json")
}

func (a *App) documentPath(tenant, path string, allowMissing bool) error {
	root, err := a.files.workspace.Root(tenant)
	if err != nil {
		return err
	}
	_, err = resolve(root, path, allowMissing)
	return friendlyPathError(err)
}
