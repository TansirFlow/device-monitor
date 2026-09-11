package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 口令派生：PBKDF2-HMAC-SHA256
//
// 本项目坚持"零第三方依赖"，而标准库直到 Go 1.24 才提供 crypto/pbkdf2 ——
// go.mod 声明的是 go 1.21，引它等于把最低 Go 版本抬到 1.24，连 Dockerfile 都要跟着改。
// PBKDF2 的本质只是"迭代 HMAC"，用 crypto/hmac 手写二十行即可，
// 且有 admin_test.go 里与 Python hashlib.pbkdf2_hmac 对拍的测试向量兜底。
// ---------------------------------------------------------------------------

const (
	pbkdf2Iterations = 210_000 // OWASP 对 PBKDF2-HMAC-SHA256 的建议量级
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
)

// pbkdf2Key 按 RFC 8018 派生密钥。block 序号与次数都是公开参数，
// 安全性完全来自 HMAC 与迭代次数，不依赖任何隐藏常量。
func pbkdf2Key(password string, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	nBlocks := (keyLen + hLen - 1) / hLen

	mac := hmac.New(sha256.New, []byte(password))
	out := make([]byte, 0, nBlocks*hLen)

	for block := 1; block <= nBlocks; block++ {
		// U1 = HMAC(password, salt || INT_32_BE(block))
		mac.Reset()
		mac.Write(salt)
		mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := mac.Sum(nil)

		t := make([]byte, hLen)
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac.Reset()
			mac.Write(u)
			sum := mac.Sum(nil)
			copy(u, sum)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// hashPassword 返回可直接落盘的编码串：算法$迭代次数$盐(hex)$派生密钥(hex)。
// 把参数写进串里，是为了以后调高迭代次数时老口令仍能校验、且可在登录时静默升级。
func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}
	dk := pbkdf2Key(password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) != pbkdf2KeyLen {
		return false
	}
	got := pbkdf2Key(password, salt, iter, pbkdf2KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------------------------------------------------------------------------
// 管理员账号
// ---------------------------------------------------------------------------

const (
	adminFileName = "admin.json"
	sessionCookie = "mon_admin_session"
	sessionTTL    = 8 * time.Hour
)

type adminAccount struct {
	Username  string `json:"username"`
	PassHash  string `json:"pass_hash"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type adminStore struct {
	mu   sync.RWMutex
	path string
	acct *adminAccount
}

func newAdminStore(dataDir string) *adminStore {
	return &adminStore{path: filepath.Join(dataDir, adminFileName)}
}

func (s *adminStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		var a adminAccount
		if err := json.Unmarshal(data, &a); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", s.path, err)
		}
		if a.Username == "" || a.PassHash == "" {
			return fmt.Errorf("%s 内容不完整（缺少 username 或 pass_hash）", s.path)
		}
		s.acct = &a
		return nil
	case os.IsNotExist(err):
		return nil
	default:
		return fmt.Errorf("读取 %s 失败: %w", s.path, err)
	}
}

func (s *adminStore) initialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.acct != nil
}

func (s *adminStore) username() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.acct == nil {
		return ""
	}
	return s.acct.Username
}

// initAccount 只在从未初始化过时生效，用于首次启动的引导。
// 已经存在账号时返回 false —— 免得有人趁 admin.json 还没建好抢先占位。
func (s *adminStore) initAccount(username, password string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acct != nil {
		return false, nil
	}
	a, err := s.buildLocked(username, password)
	if err != nil {
		return false, err
	}
	s.acct = a
	return true, s.saveLocked()
}

func (s *adminStore) setPassword(username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.buildLocked(username, password)
	if err != nil {
		return err
	}
	if s.acct != nil {
		a.CreatedAt = s.acct.CreatedAt
	}
	s.acct = a
	return s.saveLocked()
}

func (s *adminStore) buildLocked(username, password string) (*adminAccount, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("管理员用户名不能为空")
	}
	if len(username) > 64 {
		return nil, fmt.Errorf("管理员用户名过长（上限 64 字符）")
	}
	// 用户名会显示在日志与 Cookie 签发的载荷里，禁掉控制字符免得污染日志。
	for _, r := range username {
		if r < 0x20 || r == 0x7f {
			return nil, fmt.Errorf("管理员用户名不能含控制字符")
		}
	}
	if len(password) < 10 {
		return nil, fmt.Errorf("管理员密码至少 10 个字符")
	}
	if len(password) > 256 {
		return nil, fmt.Errorf("管理员密码过长（上限 256 字符）")
	}
	h, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	return &adminAccount{Username: username, PassHash: h, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *adminStore) saveLocked() error {
	data, err := json.MarshalIndent(s.acct, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(s.path, data, 0o600)
}

func (s *adminStore) verify(username, password string) bool {
	s.mu.RLock()
	acct := s.acct
	s.mu.RUnlock()
	if acct == nil {
		return false
	}
	// 用户名与口令都做恒定时间比较：用户名是否存在本身也是信息。
	nameOK := subtle.ConstantTimeCompare([]byte(username), []byte(acct.Username)) == 1
	passOK := verifyPassword(password, acct.PassHash)
	return nameOK && passOK
}

// sessionSecret 由口令哈希派生：改密码会让所有已签发会话立即失效，
// 重启则不会（secret 不放在内存随机值里），这是刻意选的行为。
func (s *adminStore) sessionSecret() []byte {
	s.mu.RLock()
	acct := s.acct
	s.mu.RUnlock()
	if acct == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(acct.PassHash))
	return sum[:]
}

// issueSession 签发形如 v1.<过期时间戳>.<用户名>.<签名> 的令牌。
// 它只是一个自包含的签名串，不需要服务端存会话：主控要保持"只写数据目录"。
func (s *adminStore) issueSession(username string, ttl time.Duration) (string, time.Time, error) {
	secret := s.sessionSecret()
	if secret == nil {
		return "", time.Time{}, fmt.Errorf("尚未初始化管理员账号")
	}
	exp := time.Now().Add(ttl)
	return signSession(secret, username, exp), exp, nil
}

func signSession(secret []byte, username string, exp time.Time) string {
	payload := "v1." + strconv.FormatInt(exp.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString([]byte(username))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// checkSession 验签并检查过期；返回会话里的用户名。
func (s *adminStore) checkSession(tok string) (string, bool) {
	secret := s.sessionSecret()
	if secret == nil || tok == "" {
		return "", false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return "", false
	}
	payload := strings.Join(parts[:3], ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	want := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, want) != 1 {
		return "", false
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() > expUnix {
		return "", false
	}
	name, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	// 会话里的用户名必须与当前账号一致：改名/换账号后旧会话作废。
	if subtle.ConstantTimeCompare(name, []byte(s.username())) != 1 {
		return "", false
	}
	return string(name), true
}

// ---------------------------------------------------------------------------
// 首次启动引导
// ---------------------------------------------------------------------------

// ensureAdmin 保证 data_dir 里有一个可用账号：
//   - 已有 admin.json → 直接用；
//   - 没有 → 用环境变量 MON_MASTER_ADMIN_PASSWORD 初始化；
//   - 环境变量也没给 → 生成随机口令并打印一次到日志。
//
// 不做"开放一个初始化页面"这种设计：那样谁先访问到谁就抢到了管理员。
// 口令只进日志（日志本身就是运维可见的），一次性、且可登录后立即修改。
func ensureAdmin(dir string) (*adminStore, error) {
	st := newAdminStore(dir)
	if err := st.load(); err != nil {
		return nil, err
	}
	if st.initialized() {
		return st, nil
	}

	username := strings.TrimSpace(os.Getenv("MON_MASTER_ADMIN_USERNAME"))
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("MON_MASTER_ADMIN_PASSWORD")
	generated := false
	if password == "" {
		var err error
		password, err = randomToken(18)
		if err != nil {
			return nil, fmt.Errorf("生成初始管理员口令失败: %w", err)
		}
		generated = true
	}
	if _, err := st.initAccount(username, password); err != nil {
		return nil, err
	}
	if generated {
		logAdminBootstrap(username, password)
	} else {
		fmt.Printf("mon-master: 已用 MON_MASTER_ADMIN_PASSWORD 初始化管理员账号 %q\n", username)
	}
	return st, nil
}

func logAdminBootstrap(username, password string) {
	fmt.Printf("mon-master: 未找到管理员账号，已自动创建初始账号（只显示这一次）\n")
	fmt.Printf("mon-master:   用户名 = %s\n", username)
	fmt.Printf("mon-master:   密　码 = %s\n", password)
	fmt.Printf("mon-master: 请登录后立即在「改密码」里改掉，或设置环境变量 MON_MASTER_ADMIN_PASSWORD 后重建 admin.json\n")
}
