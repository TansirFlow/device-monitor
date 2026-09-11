package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
)

// nodeIDRe 限制 node_id 字符集。它会被 master 用作存储路径的一段，
// 因此这里是第一道路径穿越防线（master 侧还会有第二道）。
var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CollectConfig 控制采集哪些指标。
type CollectConfig struct {
	CPU        bool     `json:"cpu"`
	Mem        bool     `json:"mem"`
	Disk       bool     `json:"disk"`
	Net        bool     `json:"net"`
	GPU        bool     `json:"gpu"`
	DiskMounts []string `json:"disk_mounts,omitempty"` // 留空=自动发现
	// PerCore 会额外上报每核占用率。64 核机器上单条样本会多出约 400 字节，
	// 长期存储会明显变大，因此默认关闭。
	PerCore bool `json:"per_core,omitempty"`

	// Temp 采集所有可读到的温度传感器（CPU / 主板 / 硬盘 / 网卡 / GPU）。
	Temp bool `json:"temp"`
	// TempLimit 限制个数，按类别优先级（CPU > GPU > 硬盘 > 主板 > 网卡）截断，
	// 避免双路主板 + 12 盘位机器一次上报上百个传感器。
	TempLimit int `json:"temp_limit,omitempty"`
	// TempExclude 按子串排除传感器，匹配 Name 或 Source。例如 ["Composite"]。
	TempExclude []string `json:"temp_exclude,omitempty"`
}

// GPUConfig 控制 GPU 采集。
type GPUConfig struct {
	Enabled     bool   `json:"enabled"`
	Binary      string `json:"binary,omitempty"` // 默认 nvidia-smi（仅支持固定参数调用）
	IncludeUUID bool   `json:"include_uuid,omitempty"`
}

// TLSConfig 控制出站 TLS。生产环境建议至少配置 CAFile 或使用系统信任链。
type TLSConfig struct {
	CAFile             string `json:"ca_file,omitempty"`
	CertFile           string `json:"cert_file,omitempty"` // 与 KeyFile 同时提供则启用 mTLS
	KeyFile            string `json:"key_file,omitempty"`
	ServerName         string `json:"server_name,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

// Config 是 agent 的全部配置。agent **只从本地文件读取配置**，
// 绝不从 master 拉取或接收任何配置/指令，这是「主控无法操作节点」的结构性保证之一。
type Config struct {
	NodeID      string            `json:"node_id"`
	MasterURL   string            `json:"master_url"`
	Token       string            `json:"token"`
	IntervalSec int               `json:"interval_sec"`
	TimeoutSec  int               `json:"timeout_sec"`
	BufferMax   int               `json:"buffer_max"`
	UseEnvProxy bool              `json:"use_env_proxy"`
	Labels      map[string]string `json:"labels,omitempty"`
	Collect     CollectConfig     `json:"collect"`
	GPU         GPUConfig         `json:"gpu"`
	TLS         TLSConfig         `json:"tls"`
}

func defaultConfig() *Config {
	return &Config{
		IntervalSec: 10,
		TimeoutSec:  8,
		BufferMax:   120,
		Collect: CollectConfig{
			CPU:       true,
			Mem:       true,
			Disk:      true,
			Net:       true,
			GPU:       true,
			Temp:      true,
			TempLimit: 32,
		},
		GPU: GPUConfig{Enabled: true, Binary: "nvidia-smi"},
	}
}

// loadConfig 读取 JSON 配置，再叠加环境变量覆盖（便于容器化部署）。
func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
	case os.IsNotExist(err):
		// 允许纯环境变量部署；如果三个关键项都由环境提供即可运行。
	default:
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	// 环境变量优先级最高，方便 K8s / Docker 注入 Secret。
	if v := strings.TrimSpace(os.Getenv("MON_NODE_ID")); v != "" {
		cfg.NodeID = v
	}
	if v := strings.TrimSpace(os.Getenv("MON_MASTER_URL")); v != "" {
		cfg.MasterURL = v
	}
	if v := os.Getenv("MON_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("MON_INTERVAL_SEC")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.IntervalSec = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("MON_LABELS")); v != "" {
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		for _, kv := range strings.Split(v, ",") {
			if k, val, ok := strings.Cut(kv, "="); ok {
				cfg.Labels[strings.TrimSpace(k)] = strings.TrimSpace(val)
			}
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.NodeID == "" {
		return fmt.Errorf("node_id 不能为空（每个节点必须唯一，仅允许字母数字 . _ -）")
	}
	if !nodeIDRe.MatchString(c.NodeID) {
		return fmt.Errorf("node_id %q 非法：仅允许 [A-Za-z0-9._-]，长度 1-64，且需以字母或数字开头", c.NodeID)
	}
	if c.MasterURL == "" {
		return fmt.Errorf("master_url 不能为空")
	}
	u, err := url.Parse(c.MasterURL)
	if err != nil {
		return fmt.Errorf("master_url 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("master_url 仅支持 http/https，当前为 %q", u.Scheme)
	}
	if u.Scheme == "http" {
		// 明文上报会泄露 token 与机器画像，必须显式确认。
		if os.Getenv("MON_ALLOW_INSECURE_HTTP") != "1" {
			return fmt.Errorf("master_url 使用明文 http 会泄露上报令牌，如确认在内网可信链路中，" +
				"请设置环境变量 MON_ALLOW_INSECURE_HTTP=1 后重试")
		}
	}
	if c.Token == "" {
		return fmt.Errorf("token 不能为空（上报令牌，仅具备上报权限）")
	}
	if len(c.Token) < 12 {
		return fmt.Errorf("token 过短（至少 12 字符），建议使用 32 字节随机串")
	}
	if c.IntervalSec < 1 {
		c.IntervalSec = 10
	}
	if c.IntervalSec > 3600 {
		return fmt.Errorf("interval_sec 过大（上限 3600）")
	}
	if c.TimeoutSec < 1 || c.TimeoutSec > 120 {
		c.TimeoutSec = 8
	}
	if c.BufferMax < 0 {
		c.BufferMax = 0
	}
	if c.BufferMax > 10000 {
		c.BufferMax = 10000 // 纯内存缓冲，加上限避免异常时吃内存
	}
	if c.GPU.Binary == "" {
		c.GPU.Binary = "nvidia-smi"
	}
	if c.Collect.TempLimit <= 0 {
		c.Collect.TempLimit = 32
	}
	if c.Collect.TempLimit > 128 {
		c.Collect.TempLimit = 128
	}
	// TempExclude 里的空白项必须真正剔除，而不是留在切片里：
	// 留在里面虽然不影响判定（excludedTemp 会跳过空串），
	// 但会让日志和配置回显出现一堆无意义的条目。
	if len(c.Collect.TempExclude) > 0 {
		cleaned := c.Collect.TempExclude[:0]
		for _, s := range c.Collect.TempExclude {
			if s = strings.TrimSpace(s); s != "" {
				cleaned = append(cleaned, s)
			}
		}
		c.Collect.TempExclude = cleaned
	}
	return nil
}

// warnFilePermissions 在类 Unix 系统上提示配置文件权限过宽。
// 配置文件里有上报令牌，不应该让同机其他用户读到。
func warnFilePermissions(path string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("配置文件 %s 权限为 %04o，同机其他用户可读，建议执行: chmod 600 %s",
			path, fi.Mode().Perm(), path)
	}
	return ""
}
