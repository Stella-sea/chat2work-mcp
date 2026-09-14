package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr           string            `yaml:"listen_addr"`
	LogLevel             string            `yaml:"log_level"`
	MCPToken             string            `yaml:"mcp_token"`
	MCPUserContextSecret string            `yaml:"mcp_user_context_secret"`
	WorkspaceRoot        string            `yaml:"workspace_root"`
	MaxFileBytes         int64             `yaml:"max_file_bytes"`
	MaxWorkspaceBytes    int64             `yaml:"max_workspace_bytes"`
	MaxWorkspaceFiles    int               `yaml:"max_workspace_files"`
	MaxListResults       int               `yaml:"max_list_results"`
	MaxSearchResults     int               `yaml:"max_search_results"`
	OfficeCLIPath        string            `yaml:"officecli_path"`
	OfficeTimeout        string            `yaml:"office_timeout"`
	MaxOfficeConcurrency int               `yaml:"max_office_concurrency"`
	DeleteEnabled        bool              `yaml:"delete_enabled"`
	StdioTenantID        string            `yaml:"stdio_tenant_id"`
	AuditLogPath         string            `yaml:"audit_log_path"`
	AuditMaxBytes        int64             `yaml:"audit_max_bytes"`
	AuditMaxBackups      int               `yaml:"audit_max_backups"`
	MaxUncompressedBytes int64             `yaml:"max_uncompressed_bytes"`
	DownloadBaseURL      string            `yaml:"download_base_url"`
	DownloadTTL          string            `yaml:"download_ttl"`
	DownloadSecret       string            `yaml:"download_secret"`
	Workspaces           []WorkspaceConfig `yaml:"workspaces"`
}

type WorkspaceConfig struct {
	ID      string            `yaml:"id"`
	Name    string            `yaml:"name"`
	Members []WorkspaceMember `yaml:"members"`
}

type WorkspaceMember struct {
	UserID uint64 `yaml:"user_id"`
	Access string `yaml:"access"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyEnv(&c)
	if c.ListenAddr == "" {
		c.ListenAddr = ":8090"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = "/data/workspace"
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 20 << 20
	}
	if c.MaxWorkspaceBytes <= 0 {
		c.MaxWorkspaceBytes = 1 << 30
	}
	if c.MaxWorkspaceFiles <= 0 {
		c.MaxWorkspaceFiles = 10000
	}
	if c.MaxListResults <= 0 {
		c.MaxListResults = 1000
	}
	if c.MaxSearchResults <= 0 {
		c.MaxSearchResults = 200
	}
	if c.OfficeCLIPath == "" {
		c.OfficeCLIPath = "officecli"
	}
	if c.OfficeTimeout == "" {
		c.OfficeTimeout = "60s"
	}
	if c.MaxOfficeConcurrency <= 0 {
		c.MaxOfficeConcurrency = 4
	}
	if c.AuditMaxBytes <= 0 {
		c.AuditMaxBytes = 50 << 20
	}
	if c.AuditMaxBackups <= 0 {
		c.AuditMaxBackups = 5
	}
	if c.MaxUncompressedBytes <= 0 {
		c.MaxUncompressedBytes = 256 << 20
	}
	if _, err := time.ParseDuration(c.OfficeTimeout); err != nil {
		return Config{}, fmt.Errorf("office_timeout: %w", err)
	}
	if c.DownloadTTL == "" {
		c.DownloadTTL = "15m"
	}
	if _, err := time.ParseDuration(c.DownloadTTL); err != nil {
		return Config{}, fmt.Errorf("download_ttl: %w", err)
	}
	if strings.TrimSpace(c.DownloadSecret) == "" {
		c.DownloadSecret = c.MCPUserContextSecret
	}
	if strings.TrimSpace(c.MCPUserContextSecret) == "" {
		return Config{}, fmt.Errorf("mcp_user_context_secret is required")
	}
	if strings.TrimSpace(c.MCPToken) == "" {
		return Config{}, fmt.Errorf("mcp_token is required")
	}
	seen := map[string]struct{}{}
	for _, workspace := range c.Workspaces {
		if !validWorkspaceID(workspace.ID) {
			return Config{}, fmt.Errorf("invalid workspace id %q", workspace.ID)
		}
		if _, ok := seen[workspace.ID]; ok {
			return Config{}, fmt.Errorf("duplicate workspace id %q", workspace.ID)
		}
		seen[workspace.ID] = struct{}{}
		for _, member := range workspace.Members {
			if member.UserID == 0 || !validAccess(member.Access) {
				return Config{}, fmt.Errorf("invalid member in workspace %q", workspace.ID)
			}
		}
	}
	return c, nil
}

func (c Config) OfficeDuration() time.Duration { d, _ := time.ParseDuration(c.OfficeTimeout); return d }

func (c Config) DownloadDuration() time.Duration { d, _ := time.ParseDuration(c.DownloadTTL); return d }

func applyEnv(c *Config) {
	set := func(key string, target *string) {
		if v, ok := os.LookupEnv(key); ok {
			*target = v
		}
	}
	set("LISTEN_ADDR", &c.ListenAddr)
	set("LOG_LEVEL", &c.LogLevel)
	set("MCP_TOKEN", &c.MCPToken)
	set("MCP_USER_CONTEXT_SECRET", &c.MCPUserContextSecret)
	set("WORKSPACE_ROOT", &c.WorkspaceRoot)
	set("OFFICECLI_PATH", &c.OfficeCLIPath)
	set("OFFICE_TIMEOUT", &c.OfficeTimeout)
	if v, ok := os.LookupEnv("MAX_OFFICE_CONCURRENCY"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxOfficeConcurrency = n
		}
	}
	set("STDIO_TENANT_ID", &c.StdioTenantID)
	set("AUDIT_LOG_PATH", &c.AuditLogPath)
	if v, ok := os.LookupEnv("AUDIT_MAX_BYTES"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.AuditMaxBytes = n
		}
	}
	if v, ok := os.LookupEnv("AUDIT_MAX_BACKUPS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.AuditMaxBackups = n
		}
	}
	if v, ok := os.LookupEnv("MAX_UNCOMPRESSED_BYTES"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.MaxUncompressedBytes = n
		}
	}
	set("DOWNLOAD_BASE_URL", &c.DownloadBaseURL)
	set("DOWNLOAD_TTL", &c.DownloadTTL)
	set("DOWNLOAD_SECRET", &c.DownloadSecret)
	if v, ok := os.LookupEnv("MAX_FILE_BYTES"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.MaxFileBytes = n
		}
	}
	if v, ok := os.LookupEnv("DELETE_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			c.DeleteEnabled = b
		}
	}
}
