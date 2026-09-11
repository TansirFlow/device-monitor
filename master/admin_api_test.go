package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 节点令牌存储
// ---------------------------------------------------------------------------

func newTestNodeStore(t *testing.T) (*nodeStore, *Config) {
	t.Helper()
	cfg := defaultConfig()
	cfg.ReportToken = "global-report-token"
	st := newNodeStore(t.TempDir(), cfg)
	if err := st.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return st, cfg
}

func TestNodeCreateAndLookup(t *testing.T) {
	st, _ := newTestNodeStore(t)
	v, err := st.create("web-01", "", "杭州机房")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.NodeID != "web-01" || len(v.Token) < 32 {
		t.Fatalf("自动生成的令牌异常: %q", v.Token)
	}
	if !v.HasCode {
		t.Fatal("新建节点应当同时生成安装码")
	}

	tk, ok := st.tokenOf("web-01")
	if !ok || tk != v.Token {
		t.Fatalf("tokenOf 取到的令牌不对: %q vs %q", tk, v.Token)
	}
	if !st.validCode("web-01", codeOf(t, st, "web-01")) {
		t.Fatal("安装码应当有效")
	}
	if st.validCode("web-01", "wrong-code") {
		t.Fatal("错误的安装码不应通过")
	}
	if st.validCode("web-01", "") {
		t.Fatal("空安装码不应通过")
	}
}

func codeOf(t *testing.T, st *nodeStore, id string) string {
	t.Helper()
	rec, ok := st.get(id)
	if !ok || rec == nil {
		t.Fatalf("找不到节点 %s", id)
	}
	return rec.InstallCode
}

func TestNodeTokenFallsBackToConfig(t *testing.T) {
	st, cfg := newTestNodeStore(t)
	// 未托管的节点回退到配置里的全局令牌
	tk, ok := st.tokenOf("anything")
	if !ok || tk != cfg.ReportToken {
		t.Fatalf("应回退到配置令牌, got %q ok=%v", tk, ok)
	}
	// 配置里声明的节点也能查到
	cfg.NodeTokens = map[string]string{"db-01": "db-token-0123456789"}
	if tk, ok := st.tokenOf("db-01"); !ok || tk != "db-token-0123456789" {
		t.Fatalf("配置里的 node_tokens 未生效: %q", tk)
	}
}

func TestNodeDisableStopsReport(t *testing.T) {
	st, _ := newTestNodeStore(t)
	if _, err := st.create("web-01", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.setDisabled("web-01", true); err != nil {
		t.Fatalf("setDisabled: %v", err)
	}
	// 停用后不能再拿它上报；同时配置里也没有它 → 未授权
	if _, ok := st.tokenOf("web-01"); ok {
		t.Fatal("停用的节点不应再返回可用令牌")
	}
	if err := st.setDisabled("web-01", false); err != nil {
		t.Fatalf("setDisabled(false): %v", err)
	}
	if _, ok := st.tokenOf("web-01"); !ok {
		t.Fatal("重新启用后应当恢复")
	}
}

func TestNodeRotateAndRevokeCode(t *testing.T) {
	st, _ := newTestNodeStore(t)
	if _, err := st.create("web-01", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	old := codeOf(t, st, "web-01")
	newCode, err := st.rotateCode("web-01")
	if err != nil {
		t.Fatalf("rotateCode: %v", err)
	}
	if newCode == old {
		t.Fatal("轮换后安装码不应相同")
	}
	if st.validCode("web-01", old) {
		t.Fatal("旧安装码应当失效")
	}
	if !st.validCode("web-01", newCode) {
		t.Fatal("新安装码应当有效")
	}
	if err := st.clearCode("web-01"); err != nil {
		t.Fatalf("clearCode: %v", err)
	}
	if st.validCode("web-01", newCode) {
		t.Fatal("撤销后安装码不应有效")
	}
	// 撤销安装码不影响上报
	if _, ok := st.tokenOf("web-01"); !ok {
		t.Fatal("撤销安装码不应影响上报令牌")
	}
}

func TestNodePersistAndReload(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	st1 := newNodeStore(dir, cfg)
	if err := st1.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	created, err := st1.create("web-01", "token-abcdefghijklmnop", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := codeOf(t, st1, "web-01")

	// 模拟重启
	st2 := newNodeStore(dir, cfg)
	if err := st2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if tk, ok := st2.tokenOf("web-01"); !ok || tk != created.Token {
		t.Fatalf("重启后令牌丢失: %q", tk)
	}
	if !st2.validCode("web-01", code) {
		t.Fatal("重启后安装码应当仍然有效")
	}

	fi, err := os.Stat(filepath.Join(dir, "nodes.json"))
	if err != nil {
		t.Fatalf("nodes.json 不存在: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 && os.Getenv("GOOS") != "windows" {
		t.Logf("提示：当前平台上 nodes.json 权限为 %04o（Windows 无 Unix 权限位属正常）", fi.Mode().Perm())
	}
}

func TestNodeValidation(t *testing.T) {
	st, _ := newTestNodeStore(t)
	// 非法节点名：路径穿越类
	for _, bad := range []string{"", "../etc", "a/b", "-x", "with space", strings.Repeat("x", 65)} {
		if _, err := st.create(bad, "", ""); err == nil {
			t.Fatalf("非法节点名 %q 应当被拒绝", bad)
		}
	}
	// 非法令牌
	for _, bad := range []string{"short", "  spaced  ", strings.Repeat("a", 300)} {
		if _, err := st.create("web-02", bad, ""); err == nil {
			t.Fatalf("非法令牌 %q 应当被拒绝", bad)
		}
	}
	// 重复创建
	if _, err := st.create("web-03", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.create("web-03", "", ""); err == nil {
		t.Fatal("重复创建同名节点应当被拒绝")
	}
}

func TestNodeTakesOverConfigNode(t *testing.T) {
	st, cfg := newTestNodeStore(t)
	cfg.NodeTokens = map[string]string{"db-01": "db-token-0123456789"}
	// 配置里声明的节点，后台可以直接接管并轮换令牌
	tk, err := st.setToken("db-01", "")
	if err != nil {
		t.Fatalf("setToken: %v", err)
	}
	if len(tk) < 32 {
		t.Fatalf("轮换后的令牌异常: %q", tk)
	}
	got, _ := st.tokenOf("db-01")
	if got != tk {
		t.Fatal("接管后应以新令牌为准")
	}
	if v := st.list(nil); len(v) != 1 {
		t.Fatalf("列表应含 1 个节点，实际 %d", len(v))
	}
}

// ---------------------------------------------------------------------------
// 安装脚本生成
// ---------------------------------------------------------------------------

func TestRenderInstallScriptLinux(t *testing.T) {
	ctx := installContext{
		NodeID:    "web-01",
		Code:      "code0123456789abcdef",
		MasterURL: "https://monitor.example.com",
		Token:     "token0123456789abcdef",
		Interval:  15,
		Generated: time.Unix(1700000000, 0).UTC(),
	}
	s, err := ctx.render("linux")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain := []string{
		`MASTER_URL='https://monitor.example.com'`,
		`NODE_ID='web-01'`,
		"mon-agent-linux-$ARCH",            // 架构自动识别
		`"node_id": "web-01"`,              // 配置里节点名
		`"token": "token0123456789abcdef"`, // 令牌已填好
		"https://monitor.example.com/api/v1/report",
		"[Unit]", "ProtectSystem=strict", // systemd 单元内联
		"systemctl enable --now mon-agent",
		"selftest",
	}
	for _, want := range mustContain {
		if !strings.Contains(s, want) {
			t.Fatalf("生成的 Linux 脚本缺少 %q", want)
		}
	}
	// 令牌必须真的落进脚本，否则"一键"无从谈起
	if strings.Contains(s, "__TOKEN__") || strings.Contains(s, "__CONFIG_JSON__") {
		t.Fatal("脚本里还有未替换的占位符")
	}
	// 加固项必须与 deploy/mon-agent.service 保持一致
	for _, hardening := range []string{
		"NoNewPrivileges=yes", "ProtectSystem=strict", "CapabilityBoundingSet=",
		"SystemCallFilter=@system-service", "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"MemoryMax=256M", "CPUQuota=20%",
	} {
		if !strings.Contains(s, hardening) {
			t.Fatalf("生成的 systemd 单元缺少加固项 %q（与 deploy/mon-agent.service 不一致？）", hardening)
		}
	}
}

func TestRenderInstallScriptWindows(t *testing.T) {
	ctx := installContext{
		NodeID:    "win-01",
		Code:      "code0123456789abcdef",
		MasterURL: "https://monitor.example.com",
		Token:     "token0123456789abcdef",
		Interval:  15,
		Generated: time.Unix(1700000000, 0).UTC(),
	}
	s, err := ctx.render("windows")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	wants := []string{
		"mon-agent-windows-$arch.exe", // 架构自动识别
		`$NodeId     = 'win-01'`,
		`"node_id": "win-01"`,
		"ExecutionTimeLimit>PT0S", // 3 天上限会把常驻进程杀掉
		"Register-ScheduledTask",
		"UTF8Encoding($false)", // 配置必须无 BOM
		"icacls",
	}
	for _, want := range wants {
		if !strings.Contains(s, want) {
			t.Fatalf("生成的 Windows 脚本缺少 %q", want)
		}
	}
}

func TestInstallCommandOmitsToken(t *testing.T) {
	ctx := installContext{NodeID: "web-01", Code: "code0123", MasterURL: "https://m.example.com", Token: "supersecrettoken"}
	sh := ctx.installCommand("linux", "https://m.example.com")
	ps := ctx.installCommand("windows", "https://m.example.com")
	if strings.Contains(sh, "supersecrettoken") || strings.Contains(ps, "supersecrettoken") {
		t.Fatal("一键命令里绝不能出现上报令牌")
	}
	if !strings.Contains(sh, "code=code0123") || !strings.Contains(ps, "code=code0123") {
		t.Fatal("一键命令里应当带安装码")
	}
}

// ---------------------------------------------------------------------------
// 管理端接口：认证、CSRF、安装码
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T) (*Server, *adminStore, *nodeStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.Listen = "127.0.0.1:0"
	cfg.ReportToken = "global-report-token-1234"
	if err := cfg.validate(); err != nil {
		t.Fatalf("config validate: %v", err)
	}
	admin := newAdminStore(dir)
	if _, err := admin.initAccount("admin", "password-12345"); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	nodes := newNodeStore(dir, cfg)
	if err := nodes.load(); err != nil {
		t.Fatalf("nodes load: %v", err)
	}
	store := NewStore(cfg.DataDir, cfg.CachePerNode)
	return NewServer(cfg, store, http.NotFoundHandler(), admin, nodes), admin, nodes
}

func doRequest(s *Server, method, path string, body string, headers map[string]string, cookie string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if cookie != "" {
		r.Header.Set("Cookie", sessionCookie+"="+cookie)
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, s *Server, admin *adminStore) string {
	t.Helper()
	tok, _, err := admin.issueSession("admin", time.Hour)
	if err != nil {
		t.Fatalf("issueSession: %v", err)
	}
	return tok
}

func TestAdminAPIRequiresSession(t *testing.T) {
	s, _, _ := newTestServer(t)
	// 未登录
	for _, p := range []string{"/api/v1/admin/nodes", "/api/v1/admin/password"} {
		w := doRequest(s, "GET", p, "", nil, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未登录时应返回 401，实际 %d", p, w.Code)
		}
	}
}

func TestAdminAPIRequiresCSRFHeader(t *testing.T) {
	s, admin, _ := newTestServer(t)
	cookie := loginCookie(t, s, admin)

	// 有会话但没有 CSRF 头 → 403（这正是 CSRF 攻击的样子）
	w := doRequest(s, "POST", "/api/v1/admin/nodes", `{"node_id":"web-01"}`, nil, cookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 头时应返回 403，实际 %d", w.Code)
	}
	// 带上自定义头就可以
	w = doRequest(s, "POST", "/api/v1/admin/nodes", `{"node_id":"web-01"}`,
		map[string]string{csrfHeader: "1"}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("带 CSRF 头时应成功，实际 %d：%s", w.Code, w.Body.String())
	}
}

func TestAdminAPIFlow(t *testing.T) {
	s, admin, nodes := newTestServer(t)
	cookie := loginCookie(t, s, admin)
	h := map[string]string{csrfHeader: "1"}

	// 1) 登录接口
	w := doRequest(s, "POST", "/api/v1/admin/login", `{"username":"admin","password":"password-12345"}`, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("登录应成功，实际 %d：%s", w.Code, w.Body.String())
	}
	var setCookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			setCookie = c.Value
			if !c.HttpOnly {
				t.Fatal("会话 Cookie 必须是 HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Fatal("会话 Cookie 必须是 SameSite=Strict")
			}
		}
	}
	if setCookie == "" {
		t.Fatal("登录未下发会话 Cookie")
	}

	// 2) 错误口令
	w = doRequest(s, "POST", "/api/v1/admin/login", `{"username":"admin","password":"wrong-password"}`, nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误口令应返回 401，实际 %d", w.Code)
	}

	// 3) 建节点 → 拿到安装命令
	w = doRequest(s, "POST", "/api/v1/admin/nodes", `{"node_id":"web-01","note":"测试"}`, h, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("建节点失败：%d %s", w.Code, w.Body.String())
	}
	var created struct {
		Node struct {
			NodeID string `json:"node_id"`
		} `json:"node"`
		Install struct {
			Linux   string `json:"linux"`
			Windows string `json:"windows"`
			ShURL   string `json:"sh_url"`
		} `json:"install"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if created.Node.NodeID != "web-01" {
		t.Fatalf("节点名不对: %q", created.Node.NodeID)
	}
	if !strings.Contains(created.Install.Linux, "code=") || !strings.Contains(created.Install.Linux, "install.sh") {
		t.Fatalf("安装命令异常: %q", created.Install.Linux)
	}

	// 4) 凭安装码取脚本：先取回 code
	rec, _ := nodes.get("web-01")
	oldToken := rec.Token // 记下字符串：rec 是指针，轮换是原地改的
	w = doRequest(s, "GET", "/api/v1/install.sh?node=web-01&code="+rec.InstallCode, "", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("取安装脚本失败：%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"node_id": "web-01"`) {
		t.Fatal("脚本里应含节点名与令牌")
	}
	// 错误安装码
	w = doRequest(s, "GET", "/api/v1/install.sh?node=web-01&code=wrong", "", nil, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("错误安装码应返回 403，实际 %d", w.Code)
	}
	// ps1 端点
	w = doRequest(s, "GET", "/api/v1/install.ps1?node=web-01&code="+rec.InstallCode, "", nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Register-ScheduledTask") {
		t.Fatalf("ps1 端点异常：%d", w.Code)
	}

	// 5) 轮换令牌
	w = doRequest(s, "POST", "/api/v1/admin/nodes/web-01/token", `{}`, h, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("轮换令牌失败：%d %s", w.Code, w.Body.String())
	}
	newRec, _ := nodes.get("web-01")
	if newRec.Token == oldToken {
		t.Fatal("令牌应当已变更")
	}

	// 6) 停用后，安装脚本也不该再发
	w = doRequest(s, "POST", "/api/v1/admin/nodes/web-01/disabled", `{"disabled":true}`, h, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("停用失败：%d", w.Code)
	}
	w = doRequest(s, "GET", "/api/v1/install.sh?node=web-01&code="+rec.InstallCode, "", nil, "")
	if w.Code != http.StatusForbidden {
		t.Fatal("停用的节点不应再发安装脚本")
	}
}

func TestAgentDownloadRequiresCodeAndAllowlist(t *testing.T) {
	s, _, nodes := newTestServer(t)

	// 准备 agent_dir
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mon-agent-linux-amd64"), []byte("fake-binary"), 0o644); err != nil {
		t.Fatalf("准备二进制: %v", err)
	}
	s.cfg.AgentDir = dir

	if _, err := nodes.create("web-01", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec, _ := nodes.get("web-01")

	// 无安装码
	w := doRequest(s, "GET", "/api/v1/agent/mon-agent-linux-amd64?node=web-01&code=nope", "", nil, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("无安装码应 403，实际 %d", w.Code)
	}
	// 正常下载
	w = doRequest(s, "GET", "/api/v1/agent/mon-agent-linux-amd64?node=web-01&code="+rec.InstallCode, "", nil, "")
	if w.Code != http.StatusOK || w.Body.String() != "fake-binary" {
		t.Fatalf("下载失败：%d %q", w.Code, w.Body.String())
	}
	// 白名单外的文件。注意 "../../../etc/passwd" 会被 ServeMux 先清洗成 301 重定向，
	// 根本到不了处理器 —— 这是第一层；处理器里的白名单与 within() 是第二、三层。
	// 因此这里只断言"绝不会 200"，具体是哪个非 2xx 码不强求。
	for _, bad := range []string{"../../../etc/passwd", "master.json", "nodes.json"} {
		w = doRequest(s, "GET", "/api/v1/agent/"+bad+"?node=web-01&code="+rec.InstallCode, "", nil, "")
		if w.Code == http.StatusOK {
			t.Fatalf("%q 不应能下载到", bad)
		}
	}
}
