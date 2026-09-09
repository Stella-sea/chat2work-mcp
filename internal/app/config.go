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
	ListenAddr           string `yaml:"listen_addr"`
	MCPToken             string `yaml:"mcp_token"`
	MCPUserContextSecret string `yaml:"mcp_user_context_secret"`
	WorkspaceRoot        string `yaml:"workspace_root"`
	MaxFileBytes         int64  `yaml:"max_file_bytes"`
	OfficeCLIPath        string `yaml:"officecli_path"`
	OfficeTimeout        string `yaml:"office_timeout"`
	DeleteEnabled        bool   `yaml:"delete_enabled"`
	StdioTenantID        string `yaml:"stdio_tenant_id"`
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
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = "/data/workspace"
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 20 << 20
	}
	if c.OfficeCLIPath == "" {
		c.OfficeCLIPath = "officecli"
	}
	if c.OfficeTimeout == "" {
		c.OfficeTimeout = "60s"
	}
	if _, err := time.ParseDuration(c.OfficeTimeout); err != nil {
		return Config{}, fmt.Errorf("office_timeout: %w", err)
	}
	if strings.TrimSpace(c.MCPUserContextSecret) == "" {
		return Config{}, fmt.Errorf("mcp_user_context_secret is required")
	}
	if strings.TrimSpace(c.MCPToken) == "" {
		return Config{}, fmt.Errorf("mcp_token is required")
	}
	return c, nil
}

func (c Config) OfficeDuration() time.Duration { d, _ := time.ParseDuration(c.OfficeTimeout); return d }

func applyEnv(c *Config) {
	set := func(key string, target *string) {
		if v, ok := os.LookupEnv(key); ok {
			*target = v
		}
	}
	set("LISTEN_ADDR", &c.ListenAddr)
	set("MCP_TOKEN", &c.MCPToken)
	set("MCP_USER_CONTEXT_SECRET", &c.MCPUserContextSecret)
	set("WORKSPACE_ROOT", &c.WorkspaceRoot)
	set("OFFICECLI_PATH", &c.OfficeCLIPath)
	set("OFFICE_TIMEOUT", &c.OfficeTimeout)
	set("STDIO_TENANT_ID", &c.StdioTenantID)
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
