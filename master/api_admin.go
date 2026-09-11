package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 管理后台接口
//
// 认证：独立的管理员账号（data_dir/admin.json）+ HMAC 签名的会话 Cookie。
// CSRF：Cookie 认证天然有 CSRF 风险，因此除了 SameSite=Strict，
//       所有写操作还要求一个自定义请求头 —— HTML 表单设不了请求头，
//       跨站 fetch 也会被 CORS 挡住，这样就彻底没有 CSRF 面了。
// 单向性：这里没有一个接口会主动去连节点；安装脚本与二进制都是**节点来取**，
//        主控只应答。
// ---------------------------------------------------------------------------

const (
	csrfHeader     = "X-Mon-Csrf"
	agentURLPrefix = "/api/v1/agent/"
)

// agentArtifacts 是允许主控分发的客户端文件白名单。
// 刻意用白名单而不是"agent_dir 下的任意文件"：那个目录里可能还放着别的运维文件，
// 不能因为开了分发端点就顺带把整个目录暴露出去。
var agentArtifacts = map[string]string{
	"mon-agent-linux-amd64":       "application/octet-stream",
	"mon-agent-linux-arm64":       "application/octet-stream",
	"mon-agent-windows-amd64.exe": "application/octet-stream",
	"mon-agent-windows-arm64.exe": "application/octet-stream",
	"mon-agent-darwin-amd64":      "application/octet-stream",
	"mon-agent-darwin-arm64":      "application/octet-stream",
}

// ---------- 会话与 CSRF ----------

func (s *Server) adminEnabled() bool {
	return s.admin != nil && s.nodes != nil
}

func (s *Server) currentAdmin(r *http.Request) string {
	if !s.adminEnabled() {
		return ""
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	user, ok := s.admin.checkSession(c.Value)
	if !ok {
		return ""
	}
	return user
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "管理后台未启用"})
		return false
	}
	if s.currentAdmin(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "需要登录管理后台"})
		return false
	}
	return true
}

func (s *Server) requireAdminWrite(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAdmin(w, r) {
		return false
	}
	if r.Header.Get(csrfHeader) != "1" {
		writeJSON(w, http.StatusForbidden,
			map[string]any{"error": "缺少 CSRF 头 " + csrfHeader + "（浏览器表单无法伪造请求头）"})
		return false
	}
	if !s.originOK(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Origin 与本站不一致"})
		return false
	}
	return true
}

// originOK 校验 Origin：浏览器跨站请求一定带 Origin，且与本站不同即为跨站。
// 同源的老式请求可能不带 Origin，此时放行（SameSite=Strict 已经挡住了大部分场景）。
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if s.cfg.TrustProxy {
		if p := firstCSV(r.Header.Get("X-Forwarded-Proto")); p != "" {
			scheme = p
		}
	}
	return strings.EqualFold(u.Host, r.Host) && strings.EqualFold(u.Scheme, scheme)
}

func firstCSV(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return strings.TrimSpace(v)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, tok string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
	})
}

func (s *Server) isSecureRequest(r *http.Request) bool {
	if s.cfg.TrustProxy {
		if p := firstCSV(r.Header.Get("X-Forwarded-Proto")); p != "" {
			return strings.EqualFold(p, "https")
		}
	}
	return r.TLS != nil
}

// publicURL 是写进安装脚本里的主控地址。
// 反代后面通常要显式配 public_base；没配时按当前请求的 scheme+Host 推断，
// 这样"在本机打开后台复制命令"也能得到能用的地址。
func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicBase != "" {
		return strings.TrimSuffix(s.cfg.PublicBase, "/")
	}
	scheme := "http"
	if s.isSecureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---------- 登录 / 登出 / 改密码 ----------

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "管理后台未启用"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": s.admin.initialized(),
		"logged_in":   s.currentAdmin(r) != "",
		"csrf_header": csrfHeader,
	})
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "管理后台未启用"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 POST"})
		return
	}
	// 登录是唯一暴露在会话之外的入口，必须单独限流：否则就是在给口令爆破开绿灯
	if !s.loginLimit.allow("login:" + s.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "登录尝试过于频繁，请稍后再试"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 解析失败"})
		return
	}
	// 用户名错误与口令错误返回同一句话：不告诉对方用户名对不对
	if !s.admin.verify(req.Username, req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或口令不正确"})
		return
	}

	tok, exp, err := s.admin.issueSession(req.Username, sessionTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "签发会话失败"})
		return
	}
	s.setSessionCookie(w, r, tok, exp)
	fmt.Printf("mon-master: 管理员 %q 登录成功（%s）\n", req.Username, s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"username": req.Username,
		"expires":  exp.Unix(),
	})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminWrite(w, r) {
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 解析失败"})
		return
	}
	who := s.currentAdmin(r)
	if !s.admin.verify(who, req.OldPassword) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "当前口令不正确"})
		return
	}
	if err := s.admin.setPassword(who, req.NewPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 改密码会让会话密钥变化 → 所有已签发会话（含当前这个）立即失效，
	// 所以这里顺手清掉 Cookie，让前端直接回到登录页。
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.isSecureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	fmt.Printf("mon-master: 管理员 %q 修改了密码，所有会话已失效\n", who)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "relogin": true})
}

// ---------- 节点与令牌 ----------

func (s *Server) handleAdminNodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.writeNodeList(w, r, http.StatusOK, nil)
	case http.MethodPost:
		if !s.requireAdminWrite(w, r) {
			return
		}
		var req struct {
			NodeID string `json:"node_id"`
			Token  string `json:"token"`
			Note   string `json:"note"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 解析失败"})
			return
		}
		view, err := s.nodes.create(strings.TrimSpace(req.NodeID), strings.TrimSpace(req.Token), req.Note)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		fmt.Printf("mon-master: 管理员 %q 创建了节点 %s\n", s.currentAdmin(r), view.NodeID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"node":    view,
			"install": s.installBlock(r, view.NodeID),
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 GET / POST"})
	}
}

// handleAdminNodeItem 处理 /api/v1/admin/nodes/<id>[/action]。
// go.mod 声明的是 go 1.21，用不了 1.22 起的 ServeMux 通配模式，只能手工解析后缀。
func (s *Server) handleAdminNodeItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/nodes/"), "/")
	if rest == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少节点名"})
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	nodeID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = strings.Trim(parts[1], "/")
	}

	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 POST / DELETE"})
		return
	}
	if !s.requireAdminWrite(w, r) {
		return
	}

	readBody := func(dst any) bool {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
			return false
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return true // 允许空体（轮换、停用这类动作不需要参数）
		}
		if err := json.Unmarshal(body, dst); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 解析失败"})
			return false
		}
		return true
	}

	switch {
	case action == "token" && r.Method == http.MethodPost:
		var req struct {
			Token string `json:"token"`
		}
		if !readBody(&req) {
			return
		}
		tk, err := s.nodes.setToken(nodeID, strings.TrimSpace(req.Token))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		fmt.Printf("mon-master: 管理员 %q 轮换/设置了节点 %s 的上报令牌\n", s.currentAdmin(r), nodeID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"token":   tk,
			"install": s.installBlock(r, nodeID),
			"hint":    "令牌已变更：已安装的节点需要重新执行安装脚本，或手工改配置后重启服务",
		})

	case action == "code" && r.Method == http.MethodPost:
		code, err := s.nodes.rotateCode(nodeID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"install": s.installBlock(r, nodeID),
			"hint":    "旧的安装命令已失效；已经在正常上报的节点不受影响",
			"code":    code,
		})

	case action == "code" && r.Method == http.MethodDelete:
		if err := s.nodes.clearCode(nodeID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hint": "安装命令已撤销"})

	case action == "disabled" && r.Method == http.MethodPost:
		var req struct {
			Disabled bool `json:"disabled"`
		}
		if !readBody(&req) {
			return
		}
		if err := s.nodes.setDisabled(nodeID, req.Disabled); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		verb := "启用"
		if req.Disabled {
			verb = "停用"
		}
		fmt.Printf("mon-master: 管理员 %q %s了节点 %s\n", s.currentAdmin(r), verb, nodeID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": req.Disabled})

	case action == "note" && r.Method == http.MethodPost:
		var req struct {
			Note string `json:"note"`
		}
		if !readBody(&req) {
			return
		}
		if err := s.nodes.setNote(nodeID, req.Note); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "" && r.Method == http.MethodDelete:
		if err := s.nodes.remove(nodeID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		fmt.Printf("mon-master: 管理员 %q 删除了节点 %s\n", s.currentAdmin(r), nodeID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "不支持的操作：" + action})
	}
}

func (s *Server) writeNodeList(w http.ResponseWriter, r *http.Request, code int, extra map[string]any) {
	now := time.Now().Unix()
	lastSeen := func(id string) int64 {
		if nv, ok := s.store.Node(id, now); ok && nv != nil {
			return nv.TS
		}
		return 0
	}
	out := map[string]any{
		"nodes":           s.nodes.list(lastSeen),
		"master_url":      s.publicURL(r),
		"agent_dir_ready": s.agentArtifactsReady(),
	}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, code, out)
}

// installBlock 返回可直接复制的安装命令与脚本地址。
// 命令里只有 node + 安装码，**不含上报令牌** —— 令牌由主控校验通过后写进脚本。
func (s *Server) installBlock(r *http.Request, nodeID string) map[string]any {
	code := ""
	if rec, ok := s.nodes.get(nodeID); ok && rec != nil {
		code = rec.InstallCode
	}
	base := s.publicURL(r)
	if code == "" {
		return map[string]any{
			"ready": false,
			"hint":  "该节点还没有安装码（可能来自配置文件），先点「轮换安装码」再复制命令",
		}
	}
	ctx := installContext{NodeID: nodeID, Code: code, MasterURL: base}
	return map[string]any{
		"ready":      true,
		"linux":      ctx.installCommand("linux", base),
		"windows":    ctx.installCommand("windows", base),
		"sh_url":     fmt.Sprintf("%s/api/v1/install.sh?node=%s&code=%s", base, nodeID, code),
		"ps1_url":    fmt.Sprintf("%s/api/v1/install.ps1?node=%s&code=%s", base, nodeID, code),
		"node_id":    nodeID,
		"code":       code,
		"master_url": base,
	}
}

// ---------- 安装脚本 ----------

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "管理后台未启用"})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 GET"})
		return
	}
	if !s.limit.allow("install:" + s.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "请求过于频繁"})
		return
	}

	nodeID := r.URL.Query().Get("node")
	code := r.URL.Query().Get("code")
	if !s.nodes.validCode(nodeID, code) {
		// 不区分"节点不存在"与"安装码不对"：不给枚举节点名的机会
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "安装码无效或已撤销。请在管理后台重新生成安装命令"})
		return
	}
	rec, _ := s.nodes.get(nodeID)
	if rec == nil || rec.Disabled {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该节点已被停用"})
		return
	}

	kind := "linux"
	if strings.HasSuffix(r.URL.Path, ".ps1") {
		kind = "windows"
	}
	if o := strings.ToLower(r.URL.Query().Get("os")); o == "windows" || o == "win" {
		kind = "windows"
	} else if o == "linux" {
		kind = "linux"
	}

	// 上报间隔沿用 agent 的默认值；想改的话装完直接改 agent.json 即可，
	// 主控侧不额外引入一个"下发配置"的概念（那会让主控变成有状态的配置中心）。
	interval := 10
	ctx := installContext{
		NodeID:    nodeID,
		Code:      code,
		MasterURL: s.publicURL(r),
		Token:     rec.Token,
		Interval:  interval,
		Generated: time.Now(),
	}
	script, err := ctx.render(kind)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "生成安装脚本失败"})
		return
	}

	fmt.Printf("mon-master: 节点 %s 下载了安装脚本（%s，来自 %s）\n", nodeID, kind, s.clientIP(r))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if kind == "windows" {
		// Windows PowerShell 5.1 靠 BOM 识别 UTF-8，否则中文会乱码
		w.Write([]byte{0xEF, 0xBB, 0xBF})
	}
	_, _ = io.WriteString(w, script)
}

// ---------- 客户端二进制分发 ----------

func (s *Server) agentArtifactsReady() bool {
	if s.cfg.AgentDir == "" {
		return false
	}
	fi, err := os.Stat(s.cfg.AgentDir)
	return err == nil && fi.IsDir()
}

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "管理后台未启用"})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 GET"})
		return
	}
	if !s.agentArtifactsReady() {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "主控没有配置 agent_dir（客户端二进制目录），无法分发。" +
				"把 dist/ 里的二进制放到某个目录，然后在 master.json 里设 agent_dir 并重启主控"})
		return
	}
	// 二进制本身不含秘密，但依然要求安装码：
	//  1) 避免这个端点变成任何人都能拉取的公开文件服务；
	//  2) 谁的机器来取过二进制，日志里有据可查。
	if !s.nodes.validCode(r.URL.Query().Get("node"), r.URL.Query().Get("code")) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "安装码无效或已撤销"})
		return
	}
	if !s.limit.allow("download:" + s.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "请求过于频繁"})
		return
	}

	name := strings.TrimPrefix(r.URL.Path, agentURLPrefix)
	name = strings.TrimPrefix(name, "/")
	// 允许 <file> 与 <file>.sha256 两种形式；文件名必须在白名单里，
	// 因此不可能出现 ../ 之类的路径穿越。
	wantChecksum := false
	if strings.HasSuffix(name, ".sha256") {
		wantChecksum = true
		name = strings.TrimSuffix(name, ".sha256")
	}
	if _, ok := agentArtifacts[name]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "不认识的文件：" + name})
		return
	}
	if wantChecksum {
		name += ".sha256"
	}

	path := filepath.Join(s.cfg.AgentDir, name)
	// 双保险：即便白名单被改坏，也不能越出分发目录
	if !within(s.cfg.AgentDir, path) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "路径越出分发目录"})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "主控的 agent_dir 里还没有这个文件：" + name})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "打开文件失败"})
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取文件信息失败"})
		return
	}
	nodeID := r.URL.Query().Get("node")
	fmt.Printf("mon-master: 节点 %s 下载了 %s（%d 字节，来自 %s）\n", nodeID, name, fi.Size(), s.clientIP(r))

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if wantChecksum {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
