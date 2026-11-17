// store.go — 账号数据的 JSON 文件存储（带读写锁与原子写入）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account 一条 muse.ai 账号记录。
// Password / APIKey 以 "enc:<base64>" 密文形态落盘。
type Account struct {
	ID         string   `json:"id"`
	Seq        uint64   `json:"seq,omitempty"`       // 内部单调递增序号，用于稳定排序
	Email      string   `json:"email"`                // 登录邮箱 / 用户名
	LoginType  string   `json:"login_type,omitempty"` // password=密码 / email_code=邮箱验证码 / both
	CodeEmail  string   `json:"code_email,omitempty"` // 接收验证码的邮箱（别名/catch-all）
	Password   string   `json:"password"`             // 登录密码（密文，验证码登录时可留空）
	APIKey     string   `json:"api_key"`              // API Key / Token（密文）
	Cookie     string   `json:"cookie"`               // muse.ai 登录 Cookie（密文，用于拉取邀请等）
	Proxy      string   `json:"proxy,omitempty"`      // 本账号请求代理（http/socks5，可选）
	InviteCode string   `json:"invite_code,omitempty"` // 邀请码 / 推荐码（同步邀请信息时自动更新）
	InviteInfo string   `json:"invite_info,omitempty"` // 最近一次邀请信息 JSON 快照
	InviteSyncedAt string `json:"invite_synced_at,omitempty"` // 快照同步时间
	CheckInfo  string   `json:"check_info,omitempty"`  // 最近准入检测结果 JSON 快照
	CheckedAt  string   `json:"checked_at,omitempty"`  // 检测时间
	RedeemInfo string   `json:"redeem_info,omitempty"` // 最近邀请码兑换链结果 JSON 快照
	RedeemedAt string   `json:"redeemed_at,omitempty"` // 兑换尝试时间
	Plan       string   `json:"plan"`                 // 订阅计划：free/plus/pro/enterprise/自定义
	Status     string   `json:"status"`               // active/expiring/banned/unverified/cooldown
	Balance    string   `json:"balance,omitempty"`    // 额度 / Token 余额（自由文本）
	ExpiresAt  string   `json:"expires_at,omitempty"` // 到期时间 YYYY-MM-DD
	Tags       []string `json:"tags,omitempty"`
	Notes      string   `json:"notes,omitempty"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

// SecretFields 需要加密保护的字段名清单。
var SecretFields = []string{"password", "api_key", "cookie"}

// DBFile 落盘文件结构。
type DBFile struct {
	Version   int        `json:"version"`
	Seq       uint64     `json:"seq"`                 // 序号计数器
	UpdatedAt string     `json:"updated_at"`
	Accounts  []*Account `json:"accounts"`
}

// Settings 全局设置（settings.json，与 accounts.json 同目录）。
type Settings struct {
	Proxy string `json:"proxy"` // 全局默认代理（http/socks5；账号未填代理时使用）
}

// Store 线程安全的 JSON 存储。
type Store struct {
	mu           sync.RWMutex
	filePath     string
	db           DBFile
	settingsPath string
	settingsMu   sync.RWMutex
	settings     Settings
}

// ErrNotFound 未找到记录。
var ErrNotFound = errors.New("记录不存在")

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

func today() string { return time.Now().Format("2006-01-02") }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "acc_" + hex.EncodeToString(b)
}

// nextSeq 分配单调递增序号（调用方需已持写锁）。
func (s *Store) nextSeq() uint64 {
	s.db.Seq++
	return s.db.Seq
}

// OpenStore 打开（不存在则创建）数据文件 <dataDir>/accounts.json。
func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	sp := filepath.Join(dataDir, "accounts.json")
	s := &Store{filePath: sp, db: DBFile{Version: 1, Accounts: []*Account{}}, settingsPath: filepath.Join(dataDir, "settings.json")}
	raw, err := os.ReadFile(sp)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if err := s.saveLocked(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.db); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", sp, err)
	}
	if s.db.Version == 0 {
		s.db.Version = 1
	}
	if s.db.Accounts == nil {
		s.db.Accounts = []*Account{}
	}
	// 加载全局设置（不存在则保持默认空值）。
	if sraw, serr := os.ReadFile(s.settingsPath); serr == nil {
		if uerr := json.Unmarshal(sraw, &s.settings); uerr != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", s.settingsPath, uerr)
		}
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return nil, serr
	}
	return s, nil
}

// Path 返回数据文件路径。
func (s *Store) Path() string { return s.filePath }

// saveLocked 调用方需已持写锁。临时文件 + rename 原子写。
func (s *Store) saveLocked() error {
	s.db.UpdatedAt = now()
	b, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

func normalize(a *Account) {
	a.Email = strings.TrimSpace(a.Email)
	a.LoginType = strings.TrimSpace(a.LoginType)
	a.CodeEmail = strings.TrimSpace(a.CodeEmail)
	a.Proxy = strings.TrimSpace(a.Proxy)
	a.Plan = strings.TrimSpace(a.Plan)
	a.Status = strings.TrimSpace(a.Status)
	a.Balance = strings.TrimSpace(a.Balance)
	a.ExpiresAt = strings.TrimSpace(a.ExpiresAt)
	a.InviteCode = strings.TrimSpace(a.InviteCode)
	a.Notes = strings.TrimSpace(a.Notes)
	if a.Tags != nil {
		out := a.Tags[:0]
		for _, t := range a.Tags {
			t = strings.TrimSpace(t)
			if t != "" {
				out = append(out, t)
			}
		}
		a.Tags = out
	}
}

func validStatus(st string) bool {
	switch st {
	case "active", "expiring", "banned", "unverified", "cooldown", "region_limited", "":
		return true
	}
	return false
}

func validLoginType(lt string) bool {
	switch lt {
	case "password", "email_code", "both", "":
		return true
	}
	return false
}

// Add 新增账号（ID/CreatedAt/UpdatedAt 由本方法生成）。Email 唯一。
func (s *Store) Add(a *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalize(a)
	if a.Email == "" {
		return fmt.Errorf("登录邮箱不能为空")
	}
	if !validStatus(a.Status) {
		return fmt.Errorf("非法状态: %s", a.Status)
	}
	if !validLoginType(a.LoginType) {
		return fmt.Errorf("非法登录方式: %s", a.LoginType)
	}
	if s.findByEmailLocked(a.Email) != nil {
		return fmt.Errorf("账号 %s 已存在", a.Email)
	}
	a.ID = newID()
	a.Seq = s.nextSeq()
	a.CreatedAt, a.UpdatedAt = now(), now()
	s.db.Accounts = append(s.db.Accounts, a)
	return s.saveLocked()
}

// Get 按 ID 查找。
func (s *Store) Get(id string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a := s.findByIDLocked(id); a != nil {
		return a, nil
	}
	return nil, ErrNotFound
}

// 	List 返回按创建顺序倒序（最新在前）的全量记录。
func (s *Store) List() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, len(s.db.Accounts))
	copy(out, s.db.Accounts)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

// Count 当前记录数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.db.Accounts)
}

// Update 按 ID 执行修改函数（已持写锁），自动刷新 UpdatedAt。
func (s *Store) Update(id string, fn func(*Account) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.findByIDLocked(id)
	if a == nil {
		return ErrNotFound
	}
	if err := fn(a); err != nil {
		return err
	}
	normalize(a)
	if a.Email == "" {
		return fmt.Errorf("登录邮箱不能为空")
	}
	if !validStatus(a.Status) {
		return fmt.Errorf("非法状态: %s", a.Status)
	}
	if !validLoginType(a.LoginType) {
		return fmt.Errorf("非法登录方式: %s", a.LoginType)
	}
	for _, o := range s.db.Accounts {
		if o != a && o.Email == a.Email {
			return fmt.Errorf("账号 %s 已存在（唯一性冲突）", a.Email)
		}
	}
	a.UpdatedAt = now()
	return s.saveLocked()
}

// Delete 按 ID 删除。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, a := range s.db.Accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	s.db.Accounts = append(s.db.Accounts[:idx], s.db.Accounts[idx+1:]...)
	return s.saveLocked()
}

// Touch 刷新最后使用时间。
func (s *Store) Touch(id string) error {
	return s.Update(id, func(a *Account) error {
		a.LastUsedAt = now()
		return nil
	})
}

// MergeImport 合并导入：ID 已存在或 Email 已存在则跳过。
// 返回（新增数, 跳过数）。
func (s *Store) MergeImport(incoming []*Account) (added, skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range incoming {
		normalize(a)
		if a.Email == "" || !validStatus(a.Status) {
			skipped++
			continue
		}
		if s.findByIDLocked(a.ID) != nil || s.findByEmailLocked(a.Email) != nil {
			skipped++
			continue
		}
		if a.ID == "" {
			a.ID = newID()
		}
		if a.Seq == 0 {
			a.Seq = s.nextSeq()
		} else if a.Seq > s.db.Seq {
			s.db.Seq = a.Seq
		}
		if a.CreatedAt == "" {
			a.CreatedAt = now()
		}
		if a.UpdatedAt == "" {
			a.UpdatedAt = now()
		}
		s.db.Accounts = append(s.db.Accounts, a)
		added++
	}
	if added > 0 {
		if err := s.saveLocked(); err != nil {
			return 0, skipped, err
		}
	}
	return added, skipped, nil
}

// ReplaceImport 整体替换导入。
func (s *Store) ReplaceImport(incoming []*Account) (imported int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := make([]*Account, 0, len(incoming))
	for _, a := range incoming {
		normalize(a)
		if a.Email == "" || !validStatus(a.Status) {
			continue
		}
		if a.ID == "" {
			a.ID = newID()
		}
		if a.Seq == 0 {
			a.Seq = s.nextSeq()
		} else if a.Seq > s.db.Seq {
			s.db.Seq = a.Seq
		}
		if a.CreatedAt == "" {
			a.CreatedAt = now()
		}
		if a.UpdatedAt == "" {
			a.UpdatedAt = now()
		}
		valid = append(valid, a)
	}
	s.db.Accounts = valid
	if err := s.saveLocked(); err != nil {
		return 0, err
	}
	return len(valid), nil
}

func (s *Store) findByIDLocked(id string) *Account {
	for _, a := range s.db.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *Store) findByEmailLocked(email string) *Account {
	email = strings.ToLower(email)
	for _, a := range s.db.Accounts {
		if strings.ToLower(a.Email) == email {
			return a
		}
	}
	return nil
}

// Snapshot 返回导出副本（含全部字段，密文形态），按创建顺序正序。
func (s *Store) Snapshot() []*Account {
	out := s.List()
	// 反转为正序（早创建在前）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// GetSettings 返回全局设置；每次从磁盘热加载（settings.json 极小），
// 使 CLI 写入（set-proxy）对运行中的服务立即生效。
func (s *Store) GetSettings() Settings {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if raw, err := os.ReadFile(s.settingsPath); err == nil {
		var st Settings
		if json.Unmarshal(raw, &st) == nil {
			s.settings = st
		}
	}
	return s.settings
}

// SaveSettings 保存全局设置（校验代理格式 + 原子写 settings.json）。
func (s *Store) SaveSettings(st Settings) error {
	st.Proxy = strings.TrimSpace(st.Proxy)
	if st.Proxy != "" {
		if err := validateProxyURL(st.Proxy); err != nil {
			return err
		}
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.settingsPath); err != nil {
		return err
	}
	s.settings = st
	return nil
}
