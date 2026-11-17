package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSettingsSaveLoadAndValidate(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 保存合法代理
	if err := s.SaveSettings(Settings{Proxy: "socks5://127.0.0.1:10808"}); err != nil {
		t.Fatalf("保存代理失败: %v", err)
	}
	// 重新打开应读到
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.GetSettings().Proxy; got != "socks5://127.0.0.1:10808" {
		t.Fatalf("应持久化代理设置, got %q", got)
	}
	// 非法代理应拒绝
	if err := s2.SaveSettings(Settings{Proxy: "ftp://bad"}); err == nil {
		t.Fatal("非法代理应报错")
	}
	if got := s2.GetSettings().Proxy; got != "socks5://127.0.0.1:10808" {
		t.Fatalf("非法保存不应改变已有设置, got %q", got)
	}
	// 清除
	if err := s2.SaveSettings(Settings{Proxy: ""}); err != nil {
		t.Fatal(err)
	}
	if got := s2.GetSettings().Proxy; got != "" {
		t.Fatalf("应支持清除代理, got %q", got)
	}
}

func TestSettingsHotReload(t *testing.T) {
	// CLI 写入 settings.json 后，已打开的 Store 实例应立即读到（服务运行中 set-proxy 生效）
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.GetSettings().Proxy; got != "" {
		t.Fatalf("初始应为空, got %q", got)
	}
	// 模拟另一个进程（CLI set-proxy）直接写盘
	other, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SaveSettings(Settings{Proxy: "socks5://127.0.0.1:10808"}); err != nil {
		t.Fatal(err)
	}
	if got := s.GetSettings().Proxy; got != "socks5://127.0.0.1:10808" {
		t.Fatalf("热加载失败（旧实例未读到新设置）, got %q", got)
	}
}

func TestAddListUpdateDelete(t *testing.T) {
	s := newTestStore(t)
	a := &Account{Email: "a@x.com", Plan: "free", Status: "active", Password: "enc:fake", Tags: []string{" t1 ", "", "t2"}}
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.CreatedAt == "" {
		t.Fatal("ID/时间应由 Store 生成")
	}
	if len(a.Tags) != 2 || a.Tags[0] != "t1" {
		t.Fatalf("标签应被清洗: %v", a.Tags)
	}
	if s.Count() != 1 {
		t.Fatal("数量应为 1")
	}
	// 唯一性
	if err := s.Add(&Account{Email: "A@X.com", Status: "active"}); err == nil {
		t.Fatal("重复邮箱（忽略大小写）应报错")
	}
	// 更新
	if err := s.Update(a.ID, func(x *Account) error { x.Status = "banned"; return nil }); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(a.ID)
	if got.Status != "banned" {
		t.Fatal("状态应已更新")
	}
	// 非法状态
	if err := s.Update(a.ID, func(x *Account) error { x.Status = "haha"; return nil }); err == nil {
		t.Fatal("非法状态应报错")
	}
	// 删除
	if err := s.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatal("删除后应为 0")
	}
	if _, err := s.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("应返回 ErrNotFound")
	}
}

func TestUpdateEmailConflict(t *testing.T) {
	s := newTestStore(t)
	a1 := &Account{Email: "one@x.com", Status: "active"}
	a2 := &Account{Email: "two@x.com", Status: "active"}
	_ = s.Add(a1)
	_ = s.Add(a2)
	err := s.Update(a2.ID, func(x *Account) error { x.Email = "one@x.com"; return nil })
	if err == nil {
		t.Fatal("更新为已存在邮箱应报错")
	}
}

func TestMergeImport(t *testing.T) {
	s := newTestStore(t)
	_ = s.Add(&Account{Email: "a@x.com", Status: "active"})
	in := []*Account{
		{ID: "acc_x1", Email: "b@x.com", Status: "active"},
		{ID: "acc_x2", Email: "a@x.com", Status: "active"},  // email 重复 → skip
		{ID: "acc_x3", Email: "c@x.com", Status: "weird"},   // 非法状态 → skip
		{Email: "d@x.com", Status: "active"},                // 无 ID → 自动生成
	}
	added, skipped, err := s.MergeImport(in)
	if err != nil || added != 2 || skipped != 2 {
		t.Fatalf("added=%d skipped=%d err=%v", added, skipped, err)
	}
	if s.Count() != 3 {
		t.Fatalf("总数应为 3，实际 %d", s.Count())
	}
}

func TestReplaceImport(t *testing.T) {
	s := newTestStore(t)
	_ = s.Add(&Account{Email: "a@x.com", Status: "active"})
	n, err := s.ReplaceImport([]*Account{{Email: "r@x.com", Status: "active"}, {Email: "", Status: "active"}})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if s.Count() != 1 || s.List()[0].Email != "r@x.com" {
		t.Fatal("replace 后应只有新账号")
	}
}

func TestTouch(t *testing.T) {
	s := newTestStore(t)
	a := &Account{Email: "a@x.com", Status: "active"}
	_ = s.Add(a)
	if err := s.Touch(a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(a.ID)
	if got.LastUsedAt == "" {
		t.Fatal("Touch 后 LastUsedAt 应非空")
	}
}

func TestLoginTypeValidation(t *testing.T) {
	s := newTestStore(t)
	if err := s.Add(&Account{Email: "a@x.com", Status: "active", LoginType: "email_code", CodeEmail: "tag@catch.com"}); err != nil {
		t.Fatalf("email_code 应合法: %v", err)
	}
	if err := s.Add(&Account{Email: "b@x.com", Status: "active", LoginType: "password"}); err != nil {
		t.Fatalf("password 应合法: %v", err)
	}
	if err := s.Add(&Account{Email: "c@x.com", Status: "active", LoginType: "fingerprint"}); err == nil {
		t.Fatal("非法登录方式应报错")
	}
	if err := s.Add(&Account{Email: "d@x.com", Status: "active"}); err != nil {
		t.Fatalf("空登录方式应合法(默认): %v", err)
	}
	a, _ := s.Get("c@x.com")
	_ = a
}

func TestListSortedByCreatedDesc(t *testing.T) {
	s := newTestStore(t)
	for _, e := range []string{"1@x.com", "2@x.com", "3@x.com"} {
		_ = s.Add(&Account{Email: e, Status: "active"})
	}
	list := s.List()
	if list[0].Email != "3@x.com" {
		t.Fatalf("应按创建时间倒序，首个=%s", list[0].Email)
	}
}
