package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 节点令牌与安装码
//
// 后台可以创建 / 轮换节点令牌，所以令牌必须"运行时可变且能持久化"。
// 存哪里是这个功能的第一个设计决定：
//
//   - **不写回 master.json**：主控改写自己的配置文件，意味着要处理并发写、
//     文件锁、以及"MON_MASTER_* 环境变量覆盖了配置"时到底谁生效的困惑。
//     主控只读配置，这条边界必须守住。
//   - 因此落到 data_dir/nodes.json（主控唯一的可写目录，systemd 单元也只放开了它）。
//   - 配置里的 report_token / node_tokens 依然有效：启动时作为**种子**合并进内存，
//     老部署不用做任何迁移；一旦在后台动过某个节点，该节点就以 nodes.json 为准。
// ---------------------------------------------------------------------------

const nodesFileName = "nodes.json"

type nodeRecord struct {
	Token       string `json:"token"`
	InstallCode string `json:"install_code,omitempty"` // 安装码，可轮换、可单独失效
	Note        string `json:"note,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type nodeFile struct {
	Nodes     map[string]*nodeRecord `json:"nodes"`
	UpdatedAt int64                  `json:"updated_at"`
}

// nodeView 是给管理后台看的投影。令牌本身也返回 ——
// 管理页是 HTTPS + 会话之内的，且运维有时需要手工填到机器上去。
type nodeView struct {
	NodeID      string `json:"node_id"`
	Token       string `json:"token"`
	HasCode     bool   `json:"has_install_code"`
	Disabled    bool   `json:"disabled"`
	Note        string `json:"note,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	Source      string `json:"source"`        // managed=后台创建或已接管；config=仅来自配置文件
	LastSeen    int64  `json:"last_seen"`     // 0 = 从未上报
	LastSeenAgo string `json:"last_seen_ago"` // 供前端直出，避免各写一遍时间格式化
}

type nodeStore struct {
	mu    sync.RWMutex
	path  string
	cfg   *Config
	nodes map[string]*nodeRecord
}

func newNodeStore(dataDir string, cfg *Config) *nodeStore {
	return &nodeStore{
		path:  filepath.Join(dataDir, nodesFileName),
		cfg:   cfg,
		nodes: map[string]*nodeRecord{},
	}
}

func (s *nodeStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		var nf nodeFile
		if err := json.Unmarshal(data, &nf); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", s.path, err)
		}
		if nf.Nodes != nil {
			s.nodes = nf.Nodes
		}
	case os.IsNotExist(err):
	default:
		return fmt.Errorf("读取 %s 失败: %w", s.path, err)
	}

	// 清洗：磁盘上的东西一律不可信（可能被人手改过）
	for id, rec := range s.nodes {
		if !nodeIDRe.MatchString(id) || rec == nil {
			delete(s.nodes, id)
			continue
		}
		if len(rec.Note) > 200 {
			rec.Note = rec.Note[:200]
		}
	}
	return nil
}

// tokenOf 返回该节点当前应使用的上报令牌。
// 顺序：后台托管的记录（未停用）→ 配置文件。这样后台新增的令牌立即生效，不用重启。
func (s *nodeStore) tokenOf(nodeID string) (string, bool) {
	s.mu.RLock()
	rec, ok := s.nodes[nodeID]
	s.mu.RUnlock()
	if ok && rec != nil {
		// 后台接管过的节点一律以后台为准，**不再回退到配置**：
		// 否则主控只要还配着全局 report_token，后台里"停用"这个节点就形同虚设
		// （它会拿着全局令牌继续上报成功）。
		if rec.Disabled || rec.Token == "" {
			return "", false
		}
		return rec.Token, true
	}
	return s.cfg.tokenOf(nodeID)
}

// validCode 校验安装码。安装码独立于上报令牌，只用于换取安装脚本，
// 可以随时轮换而不影响已经在跑的节点。
func (s *nodeStore) validCode(nodeID, code string) bool {
	if code == "" {
		return false
	}
	s.mu.RLock()
	rec, ok := s.nodes[nodeID]
	s.mu.RUnlock()
	if !ok || rec == nil || rec.Disabled || rec.InstallCode == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(code), []byte(rec.InstallCode)) == 1
}

func (s *nodeStore) exists(nodeID string) bool {
	s.mu.RLock()
	_, managed := s.nodes[nodeID]
	s.mu.RUnlock()
	if managed {
		return true
	}
	_, ok := s.cfg.NodeTokens[nodeID]
	return ok || s.cfg.ReportToken != ""
}

// list 给后台列出所有节点：托管记录 + 配置文件里已声明的节点。
func (s *nodeStore) list(lastSeen func(string) int64) []nodeView {
	s.mu.RLock()
	ids := make([]string, 0, len(s.nodes)+len(s.cfg.NodeTokens))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	for id := range s.cfg.NodeTokens {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]nodeView, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true

		s.mu.RLock()
		rec := s.nodes[id]
		s.mu.RUnlock()

		v := nodeView{NodeID: id, Source: "config"}
		if rec != nil {
			v.Source = "managed"
			v.Token = rec.Token
			v.HasCode = rec.InstallCode != ""
			v.Disabled = rec.Disabled
			v.Note = rec.Note
			v.CreatedAt = rec.CreatedAt
			v.UpdatedAt = rec.UpdatedAt
		} else if tk, ok := s.cfg.NodeTokens[id]; ok {
			v.Token = tk
		} else {
			v.Token = s.cfg.ReportToken
		}
		if lastSeen != nil {
			v.LastSeen = lastSeen(id)
			v.LastSeenAgo = humanAgo(v.LastSeen)
		}
		out = append(out, v)
	}
	return out
}

func (s *nodeStore) get(nodeID string) (*nodeRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.nodes[nodeID]
	return rec, ok
}

// create 新建节点：不指定 token 就随机生成一个。
func (s *nodeStore) create(nodeID, token, note string) (nodeView, error) {
	if !nodeIDRe.MatchString(nodeID) {
		return nodeView{}, fmt.Errorf("节点名 %q 不合法：仅允许 [A-Za-z0-9._-]，1-64 字符，且以字母或数字开头", nodeID)
	}
	if token == "" {
		var err error
		if token, err = randomToken(32); err != nil {
			return nodeView{}, err
		}
	}
	if err := validateNodeToken(token); err != nil {
		return nodeView{}, err
	}
	if len(note) > 200 {
		note = note[:200]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.nodes[nodeID]; ok && rec != nil && !rec.Disabled {
		return nodeView{}, fmt.Errorf("节点 %s 已存在", nodeID)
	}
	now := time.Now().Unix()
	code, err := randomToken(24)
	if err != nil {
		return nodeView{}, err
	}
	s.nodes[nodeID] = &nodeRecord{
		Token:       token,
		InstallCode: code,
		Note:        note,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.saveLocked(); err != nil {
		return nodeView{}, err
	}
	return nodeView{NodeID: nodeID, Token: token, HasCode: true, Note: note,
		CreatedAt: now, UpdatedAt: now, Source: "managed"}, nil
}

// setToken 手动设置或重新随机生成令牌。
func (s *nodeStore) setToken(nodeID, token string) (string, error) {
	if token == "" {
		var err error
		if token, err = randomToken(32); err != nil {
			return "", err
		}
	}
	if err := validateNodeToken(token); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.mutableLocked(nodeID)
	if err != nil {
		return "", err
	}
	rec.Token = token
	rec.UpdatedAt = time.Now().Unix()
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return token, nil
}

// rotateCode 轮换安装码：旧的安装命令立刻失效，已经在跑的节点不受影响。
func (s *nodeStore) rotateCode(nodeID string) (string, error) {
	code, err := randomToken(24)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.mutableLocked(nodeID)
	if err != nil {
		return "", err
	}
	rec.InstallCode = code
	rec.UpdatedAt = time.Now().Unix()
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return code, nil
}

// clearCode 撤销安装码（不再允许任何人凭它换取安装脚本）。
func (s *nodeStore) clearCode(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.mutableLocked(nodeID)
	if err != nil {
		return err
	}
	rec.InstallCode = ""
	rec.UpdatedAt = time.Now().Unix()
	return s.saveLocked()
}

func (s *nodeStore) setDisabled(nodeID string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.mutableLocked(nodeID)
	if err != nil {
		return err
	}
	rec.Disabled = disabled
	rec.UpdatedAt = time.Now().Unix()
	return s.saveLocked()
}

func (s *nodeStore) setNote(nodeID, note string) error {
	if len(note) > 200 {
		note = note[:200]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.mutableLocked(nodeID)
	if err != nil {
		return err
	}
	rec.Note = strings.TrimSpace(note)
	rec.UpdatedAt = time.Now().Unix()
	return s.saveLocked()
}

// remove 彻底删除托管记录。若该节点只存在于配置文件里，
// 删掉后重启仍会被种子重新载入 —— 这种情况后台会提示去改配置。
func (s *nodeStore) remove(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[nodeID]; !ok {
		return fmt.Errorf("节点 %s 不在托管列表里（它可能来自配置文件）", nodeID)
	}
	delete(s.nodes, nodeID)
	return s.saveLocked()
}

// mutableLocked 取出一条**可写**的记录；配置文件里声明的节点会被"接管"
// （复制成托管记录），这样后台才可能对它轮换令牌或停用。
func (s *nodeStore) mutableLocked(nodeID string) (*nodeRecord, error) {
	if !nodeIDRe.MatchString(nodeID) {
		return nil, fmt.Errorf("节点名 %q 不合法", nodeID)
	}
	if rec, ok := s.nodes[nodeID]; ok && rec != nil {
		return rec, nil
	}
	tk, ok := s.cfg.NodeTokens[nodeID]
	if !ok {
		if s.cfg.ReportToken != "" {
			tk = s.cfg.ReportToken
		} else {
			return nil, fmt.Errorf("节点 %s 不存在，请先在后台创建它", nodeID)
		}
	}
	now := time.Now().Unix()
	rec := &nodeRecord{Token: tk, CreatedAt: now, UpdatedAt: now}
	s.nodes[nodeID] = rec
	return rec, nil
}

func validateNodeToken(token string) error {
	if strings.TrimSpace(token) != token || token == "" {
		return fmt.Errorf("令牌不能为空，且首尾不能有空白")
	}
	if len(token) < 12 {
		return fmt.Errorf("令牌至少 12 个字符（建议 32 字节随机串）")
	}
	if len(token) > 256 {
		return fmt.Errorf("令牌过长（上限 256 字符）")
	}
	for _, r := range token {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("令牌不能含控制字符")
		}
	}
	return nil
}

func (s *nodeStore) saveLocked() error {
	nf := nodeFile{Nodes: s.nodes, UpdatedAt: time.Now().Unix()}
	data, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// 600：这里面全是节点的上报令牌，不能让同机其他用户读到
	return writeFileAtomic(s.path, data, 0o600)
}

// humanAgo 把"最近一次上报"格式化成中文短语，前端直接显示。
func humanAgo(ts int64) string {
	if ts <= 0 {
		return "从未上报"
	}
	d := time.Now().Unix() - ts
	switch {
	case d < 0:
		return "刚刚"
	case d < 60:
		return fmt.Sprintf("%d 秒前", d)
	case d < 3600:
		return fmt.Sprintf("%d 分钟前", d/60)
	case d < 86400:
		return fmt.Sprintf("%d 小时前", d/3600)
	default:
		return fmt.Sprintf("%d 天前", d/86400)
	}
}
