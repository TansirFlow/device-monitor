package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// PBKDF2
//
// 标准库直到 Go 1.24 才有 crypto/pbkdf2，而 go.mod 声明的是 go 1.21，
// 引它就是把最低 Go 版本抬到 1.24。所以这里手写了 PBKDF2-HMAC-SHA256。
// 手写密码学代码必须有外部对拍，下面这些向量的期望值是用
// Python 的 hashlib.pbkdf2_hmac('sha256', ...) 独立算出来并逐字节比对过的，
// 覆盖：单块 / 多块（40 字节跨两个块）/ 不同迭代次数 / UTF-8 中文口令。
// ---------------------------------------------------------------------------

func TestPBKDF2KnownVectors(t *testing.T) {
	cases := []struct {
		name         string
		pass, salt   string
		iter, keyLen int
		wantHex      string
	}{
		{
			name: "1次迭代",
			pass: "password", salt: "salt", iter: 1, keyLen: 32,
			wantHex: "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b",
		},
		{
			name: "2次迭代",
			pass: "password", salt: "salt", iter: 2, keyLen: 32,
			wantHex: "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43",
		},
		{
			name: "4096次迭代",
			pass: "password", salt: "salt", iter: 4096, keyLen: 32,
			wantHex: "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a",
		},
		{
			name: "生产用的210000次迭代",
			pass: "password", salt: "salt", iter: 210000, keyLen: 32,
			wantHex: "9d5f68774306eaee6c79c5b4d3a263907f81c55d4daa1c50585e1e849065e090",
		},
		{
			// 40 字节 > SHA256 的 32 字节输出，会走第二个分块，最容易写错
			name: "跨两个分块_40字节",
			pass: "passwordPASSWORDpassword", salt: "saltSALTsaltSALTsaltSALTsaltSALTsalt",
			iter: 4096, keyLen: 40,
			wantHex: "348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9",
		},
		{
			name: "含符号的口令",
			pass: "p@ssw0rd'!", salt: "0123456789abcdef", iter: 1000, keyLen: 32,
			wantHex: "360a5ca9314ba2d7627f3d07aedd602b7af8d6d709ec10e4e4e8b476a003b07b",
		},
		{
			name: "UTF8中文口令",
			pass: "中文口令", salt: "salt", iter: 1000, keyLen: 32,
			wantHex: "a39c4cbff54dabcd79dd8d107b7d18f84acd3c2f30c248329588672336e4426c",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hex.EncodeToString(pbkdf2Key(c.pass, []byte(c.salt), c.iter, c.keyLen))
			if got != c.wantHex {
				t.Fatalf("pbkdf2 结果不符\n got  %s\n want %s", got, c.wantHex)
			}
		})
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !verifyPassword("correct horse battery staple", h) {
		t.Fatal("正确口令应当校验通过")
	}
	if verifyPassword("correct horse battery staples", h) {
		t.Fatal("错误口令不应当通过")
	}
	if verifyPassword("", h) {
		t.Fatal("空口令不应当通过")
	}

	// 每次都要用不同的盐
	h2, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if h == h2 {
		t.Fatal("两次哈希结果相同，说明盐没有随机化")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"pbkdf2-sha256",
		"pbkdf2-sha256$abc$00$00",        // 迭代次数不是数字
		"pbkdf2-sha256$1000$zz$00",       // 盐不是 hex
		"other-kdf$1000$00$00",           // 算法名不对
		"pbkdf2-sha256$1000$00$0011",     // 密钥长度不对
		"pbkdf2-sha256$1000$00$00$extra", // 字段过多
	}
	for _, b := range bad {
		if verifyPassword("x", b) {
			t.Fatalf("畸形哈希 %q 不应通过校验", b)
		}
	}
}

// ---------------------------------------------------------------------------
// 账号与会话
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T) *adminStore {
	t.Helper()
	st := newAdminStore(t.TempDir())
	return st
}

func TestAdminInitOnlyOnce(t *testing.T) {
	st := newTestStore(t)
	if st.initialized() {
		t.Fatal("新目录不应已初始化")
	}
	ok, err := st.initAccount("admin", "initial-password")
	if err != nil || !ok {
		t.Fatalf("首次初始化失败: ok=%v err=%v", ok, err)
	}
	// 第二次必须被拒绝：否则有人能在 admin.json 建好之前抢先占位
	ok, err = st.initAccount("hacker", "another-password")
	if err != nil {
		t.Fatalf("重复初始化不应报错: %v", err)
	}
	if ok {
		t.Fatal("账号已存在时不应再次初始化")
	}
	if st.username() != "admin" {
		t.Fatalf("用户名被改成了 %q", st.username())
	}
	if !st.verify("admin", "initial-password") {
		t.Fatal("初始口令应可用")
	}
	if st.verify("hacker", "another-password") {
		t.Fatal("抢占者的口令不应生效")
	}
}

func TestAdminPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st1 := newAdminStore(dir)
	if _, err := st1.initAccount("ops", "password-12345"); err != nil {
		t.Fatalf("初始化: %v", err)
	}
	// 模拟重启：新建 store 重新 load
	st2 := newAdminStore(dir)
	if err := st2.load(); err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	if !st2.verify("ops", "password-12345") {
		t.Fatal("重启后口令应当仍然有效")
	}
	// 会话密钥由口令哈希派生，因此重启后旧会话依然有效（这是刻意的行为）
	tok, _, err := st1.issueSession("ops", time.Hour)
	if err != nil {
		t.Fatalf("签发会话: %v", err)
	}
	if _, ok := st2.checkSession(tok); !ok {
		t.Fatal("重启后旧会话应当仍然有效")
	}
}

func TestAdminAccountFileMode(t *testing.T) {
	dir := t.TempDir()
	st := newAdminStore(dir)
	if _, err := st.initAccount("admin", "password-12345"); err != nil {
		t.Fatalf("初始化: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "admin.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS == "windows" {
		// Windows 的 chmod 只有"只读位"这一档，表达不了 Unix 的 0600
		// （会报 0666），断言权限没有意义 —— 与 agent 的 warnFilePermissions 同理。
		t.Skip("Windows 无 Unix 权限位，跳过权限断言")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("admin.json 权限过宽: %04o（同机其他用户不应可读）", perm)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.initAccount("admin", "password-12345"); err != nil {
		t.Fatalf("初始化: %v", err)
	}

	tok, exp, err := st.issueSession("admin", time.Hour)
	if err != nil {
		t.Fatalf("签发: %v", err)
	}
	if time.Until(exp) <= 0 {
		t.Fatal("过期时间应在未来")
	}
	user, ok := st.checkSession(tok)
	if !ok || user != "admin" {
		t.Fatalf("会话校验失败: user=%q ok=%v", user, ok)
	}

	// 篡改签名
	parts := strings.Split(tok, ".")
	if len(parts) != 4 {
		t.Fatalf("会话格式异常: %v", parts)
	}
	if _, ok := st.checkSession(strings.Join(parts[:3], ".") + ".AAAA"); ok {
		t.Fatal("签名被篡改的会话不应通过")
	}
	// 篡改载荷（改用户名）
	if _, ok := st.checkSession("v1." + parts[1] + "." + parts[2] + "x." + parts[3]); ok {
		t.Fatal("载荷被篡改的会话不应通过")
	}
	// 空会话 / 畸形会话
	if _, ok := st.checkSession(""); ok {
		t.Fatal("空会话不应通过")
	}
	if _, ok := st.checkSession("garbage"); ok {
		t.Fatal("畸形会话不应通过")
	}
}

func TestSessionExpired(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.initAccount("admin", "password-12345"); err != nil {
		t.Fatalf("初始化: %v", err)
	}
	secret := st.sessionSecret()
	tok := signSession(secret, "admin", time.Now().Add(-time.Minute))
	if _, ok := st.checkSession(tok); ok {
		t.Fatal("过期会话不应通过")
	}
}

func TestChangePasswordInvalidatesSessions(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.initAccount("admin", "password-12345"); err != nil {
		t.Fatalf("初始化: %v", err)
	}
	tok, _, err := st.issueSession("admin", time.Hour)
	if err != nil {
		t.Fatalf("签发: %v", err)
	}
	if _, ok := st.checkSession(tok); !ok {
		t.Fatal("改密前会话应有效")
	}
	if err := st.setPassword("admin", "new-password-999"); err != nil {
		t.Fatalf("改密: %v", err)
	}
	if _, ok := st.checkSession(tok); ok {
		t.Fatal("改密后旧会话必须失效（会话密钥由口令哈希派生）")
	}
	if !st.verify("admin", "new-password-999") {
		t.Fatal("新口令应可用")
	}
	if st.verify("admin", "password-12345") {
		t.Fatal("旧口令不应再可用")
	}
}

func TestSessionBoundToCurrentUsername(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.initAccount("admin", "password-12345"); err != nil {
		t.Fatalf("初始化: %v", err)
	}
	tok, _, err := st.issueSession("admin", time.Hour)
	if err != nil {
		t.Fatalf("签发: %v", err)
	}
	// 换一个用户名后，旧会话里的用户名与当前账号不一致 → 作废
	if err := st.setPassword("root", "password-12345"); err != nil {
		t.Fatalf("改账号: %v", err)
	}
	if _, ok := st.checkSession(tok); ok {
		t.Fatal("账号名变化后旧会话应失效")
	}
}

func TestWeakPasswordRejected(t *testing.T) {
	st := newTestStore(t)
	for _, p := range []string{"", "123", "short", "123456789"} {
		if _, err := st.initAccount("admin", p); err == nil {
			t.Fatalf("弱口令 %q 应当被拒绝", p)
		}
	}
	if _, err := st.initAccount("admin", "at-least-10"); err != nil {
		t.Fatalf("10 字符口令应当被接受: %v", err)
	}
	// 用户名里的控制字符会污染日志
	if err := st.setPassword("bad\nname", "password-12345"); err == nil {
		t.Fatal("含控制字符的用户名应当被拒绝")
	}
}
