package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

// Config 是 master 的全部配置。
//
// 注意这里**不存在**任何指向节点的地址、端口、凭据或回调字段——
// 这不是"没配置"，而是设计上就不提供：主控没有通往节点的手段。
type Config struct {
	Listen     string `json:"listen"`      // 默认 127.0.0.1:8080
	DataDir    string `json:"data_dir"`    // 默认 ./data
	Title      string `json:"title"`       // 看板标题
	PublicBase string `json:"public_base"` // 反代对外地址；也用作生成安装脚本时的主控地址

	// AgentDir 存放待分发的客户端二进制（mon-agent-linux-amd64 等）。
	// 为空则「一键安装」只生成脚本、不提供二进制下载。
	AgentDir string `json:"agent_dir"`

	// 上报鉴权：NodeTokens 非空时使用「一节点一令牌」，否则回退到全局 ReportToken。
	ReportToken string            `json:"report_token"`
	NodeTokens  map[string]string `json:"node_tokens"`

	// 读取鉴权：看板与查询接口。为空则仅允许本机监听地址。
	AdminToken string `json:"admin_token"`

	RetentionDays   int     `json:"retention_days"`
	CachePerNode    int     `json:"cache_per_node"`
	MaxBodyKB       int     `json:"max_body_kb"`
	RateLimitPerSec float64 `json:"rate_limit_per_sec"`
	TrustProxy      bool    `json:"trust_proxy"`
}

func defaultConfig() *Config {
	return &Config{
		Listen:          "127.0.0.1:8080",
		DataDir:         "./data",
		Title:           "服务器监控",
		RetentionDays:   30,
		CachePerNode:    1440,
		MaxBodyKB:       256,
		RateLimitPerSec: 5,
	}
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	env := func(k string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			*dst = v
		}
	}
	env("MON_MASTER_LISTEN", &cfg.Listen)
	env("MON_MASTER_DATA_DIR", &cfg.DataDir)
	env("MON_MASTER_REPORT_TOKEN", &cfg.ReportToken)
	env("MON_MASTER_ADMIN_TOKEN", &cfg.AdminToken)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen 地址 %q 不合法，应形如 127.0.0.1:8080: %w", c.Listen, err)
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.RetentionDays < 1 {
		c.RetentionDays = 30
	}
	if c.RetentionDays > 3650 {
		c.RetentionDays = 3650
	}
	if c.CachePerNode < 60 {
		c.CachePerNode = 60
	}
	if c.MaxBodyKB < 16 {
		c.MaxBodyKB = 16
	}
	if c.MaxBodyKB > 4096 {
		c.MaxBodyKB = 4096
	}
	if c.RateLimitPerSec <= 0 {
		c.RateLimitPerSec = 5
	}
	if c.Title == "" {
		c.Title = "服务器监控"
	}
	if c.AgentDir != "" {
		c.AgentDir = strings.TrimSpace(c.AgentDir)
		fi, err := os.Stat(c.AgentDir)
		if err != nil {
			return fmt.Errorf("agent_dir %q 不可用: %w", c.AgentDir, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("agent_dir %q 不是一个目录", c.AgentDir)
		}
	}

	if c.ReportToken == "" && len(c.NodeTokens) == 0 {
		return fmt.Errorf("必须配置 report_token 或 node_tokens，否则任何来源都能写入数据")
	}
	if c.ReportToken != "" && len(c.ReportToken) < 12 {
		return fmt.Errorf("report_token 至少 12 个字符")
	}
	for id, tk := range c.NodeTokens {
		if !nodeIDRe.MatchString(id) {
			return fmt.Errorf("node_tokens 中的节点名 %q 不合法", id)
		}
		if len(tk) < 12 {
			return fmt.Errorf("节点 %s 的令牌至少 12 个字符", id)
		}
	}
	if c.AdminToken != "" && len(c.AdminToken) < 12 {
		return fmt.Errorf("admin_token 至少 12 个字符")
	}

	// 安全兜底：监听在非回环地址且没有读取令牌时，等于把整机房资产画像
	// 公开给任何能连上的主机。必须显式确认才能这么干。
	if !c.isLoopbackListen() && c.AdminToken == "" {
		if os.Getenv("MON_MASTER_ALLOW_OPEN_READ") != "1" {
			return fmt.Errorf("监听地址 %s 不是回环地址，但未设置 admin_token。"+
				"这会让任何能访问该端口的人看到全部节点数据。请设置 admin_token，"+
				"或（不推荐）设置环境变量 MON_MASTER_ALLOW_OPEN_READ=1 以确认该风险", c.Listen)
		}
	}
	return nil
}

func (c *Config) isLoopbackListen() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "" { // ":8080" 等价于监听全部网卡
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tokenOf 返回某节点应使用的上报令牌。
func (c *Config) tokenOf(nodeID string) (string, bool) {
	if len(c.NodeTokens) > 0 {
		tk, ok := c.NodeTokens[nodeID]
		return tk, ok
	}
	return c.ReportToken, c.ReportToken != ""
}
