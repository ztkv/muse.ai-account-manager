package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSelectedAccounts(t *testing.T) {
	st := newTestStore(t)
	if err := st.Add(&Account{Email: "a@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&Account{Email: "b@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st}
	all := srv.selectedAccounts(nil)
	if len(all) != 2 {
		t.Fatalf("空 ids 应返回全部: %d", len(all))
	}
	// 指定 1 个 + 重复 + 不存在
	got := srv.selectedAccounts([]string{all[0].ID, all[0].ID, "acc_not_exist"})
	if len(got) != 1 || got[0].ID != all[0].ID {
		t.Fatalf("应按 ids 过滤并去重/忽略不存在: %+v", got)
	}
}

// TestCheckAllWithIDs 体检批量接口支持 {"ids":[...]} 限定范围。
func TestCheckAllWithIDs(t *testing.T) {
	st := newTestStore(t)
	if err := st.Add(&Account{Email: "one@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&Account{Email: "two@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	cr, _ := NewCrypto(make([]byte, 32))
	srv := &Server{store: st, crypto: cr}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	list := st.List()
	body, _ := json.Marshal(map[string]any{"ids": []string{list[0].ID}})
	resp, err := http.Post(ts.URL+"/api/accounts/check-all", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var parsed struct {
		Data struct {
			Results []checkOutcome `json:"results"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	if len(parsed.Data.Results) != 1 {
		t.Fatalf("限定 ids 应只检测 1 个: %d", len(parsed.Data.Results))
	}
	if parsed.Data.Results[0].ID != list[0].ID {
		t.Fatalf("检测对象错误: %+v", parsed.Data.Results[0])
	}
}

// TestFetchInviteAllWithIDs 邀请批量接口支持 {"ids":[...]}。
func TestFetchInviteAllWithIDs(t *testing.T) {
	st := newTestStore(t)
	if err := st.Add(&Account{Email: "one@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&Account{Email: "two@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	cr, _ := NewCrypto(make([]byte, 32))
	srv := &Server{store: st, crypto: cr}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	list := st.List()
	body, _ := json.Marshal(map[string]any{"ids": []string{list[1].ID}})
	resp, err := http.Post(ts.URL+"/api/accounts/fetch-invite-all", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var parsed struct {
		Data struct {
			Results []inviteRefreshOutcome `json:"results"`
			Summary inviteRefreshSummary   `json:"summary"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	if len(parsed.Data.Results) != 1 || parsed.Data.Results[0].ID != list[1].ID {
		t.Fatalf("限定 ids 应只刷新 1 个: %+v", parsed.Data.Results)
	}
	if parsed.Data.Summary.Total != 1 {
		t.Fatalf("汇总 total 应为 1: %+v", parsed.Data.Summary)
	}
}