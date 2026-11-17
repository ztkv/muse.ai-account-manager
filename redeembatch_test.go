package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMapRedeemStatus(t *testing.T) {
	cases := map[string]string{
		"redeemed":       "active",
		"auth_expired":   "unverified",
		"region_limited": "region_limited",
		"already_redeemed": "",
		"used_up":        "",
		"invalid":        "",
		"error":          "",
		"http-500":       "",
		"":               "",
	}
	for state, want := range cases {
		if got := mapRedeemStatus(state); got != want {
			t.Fatalf("mapRedeemStatus(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestAggregateRedeemBatch(t *testing.T) {
	outs := []redeemBatchOutcome{
		{Email: "a@x.com", OK: true, State: "redeemed", OwnCode: "AA1"},
		{Email: "b@x.com", OK: true, State: "redeemed"},
		{Email: "c@x.com", OK: true, State: "already_redeemed"},
		{Email: "d@x.com", OK: true, State: "used_up"},
		{Email: "e@x.com", OK: true, State: "invalid"},
		{Email: "f@x.com", OK: true, State: "auth_expired"},
		{Email: "g@x.com", OK: true, State: "region_limited"},
		{Email: "h@x.com", OK: false, Error: "网络失败"},
		{Email: "i@x.com", OK: true, State: "http-502"},
		{Email: "j@x.com", Skipped: true, Error: "无 Cookie"},
	}
	sum := aggregateRedeemBatch(outs)
	if sum.Total != 10 || sum.Succeeded != 2 || sum.AlreadyRedeemed != 1 || sum.UsedUp != 1 ||
		sum.Invalid != 1 || sum.AuthExpired != 1 || sum.RegionLimited != 1 || sum.Failed != 2 || sum.Skipped != 1 {
		t.Fatalf("汇总计数错误: %+v", sum)
	}
}

func TestAggregateRedeemBatchEmpty(t *testing.T) {
	if sum := aggregateRedeemBatch(nil); sum.Total != 0 {
		t.Fatalf("空列表应为零值: %+v", sum)
	}
}

// TestRedeemBatchRouteValidation 参数校验：空码 / 空 ids / 非法请求体。
func TestRedeemBatchRouteValidation(t *testing.T) {
	st := newTestStore(t)
	cr, _ := NewCrypto(make([]byte, 32))
	srv := &Server{store: st, crypto: cr}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/api/accounts/redeem-batch", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(`{"code":"","ids":["x"]}`); c != http.StatusBadRequest {
		t.Fatalf("空码应 400, got %d", c)
	}
	if c := post(`{"code":"AB12CD","ids":[]}`); c != http.StatusBadRequest {
		t.Fatalf("空 ids 应 400, got %d", c)
	}
	if c := post(`not-json`); c != http.StatusBadRequest {
		t.Fatalf("非法请求体应 400, got %d", c)
	}
}

// TestRedeemBatchRouteSkipped 无 Cookie 账号 → skipped，不发起网络请求；
// 不存在的账号 → failed（账号不存在）。
func TestRedeemBatchRouteSkipped(t *testing.T) {
	st := newTestStore(t)
	if err := st.Add(&Account{Email: "nocookie@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	cr, _ := NewCrypto(make([]byte, 32))
	srv := &Server{store: st, crypto: cr}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ids := make([]string, 0, 2)
	for _, a := range st.List() {
		ids = append(ids, a.ID)
	}
	ids = append(ids, "acc_not_exist")
	body, _ := json.Marshal(map[string]any{"code": "TESTCODE", "ids": ids})
	resp, err := http.Post(ts.URL+"/api/accounts/redeem-batch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200, got %d", resp.StatusCode)
	}
	var parsed struct {
		OK   bool `json:"ok"`
		Data struct {
			Code    string               `json:"code"`
			Results []redeemBatchOutcome `json:"results"`
			Summary redeemBatchSummary   `json:"summary"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.OK || parsed.Data.Code != "TESTCODE" {
		t.Fatalf("应返回 ok 与规范化大写码: %+v", parsed)
	}
	sum := parsed.Data.Summary
	if sum.Total != 2 || sum.Skipped != 1 || sum.Failed != 1 || sum.Succeeded != 0 {
		t.Fatalf("汇总应为 total=2 skipped=1 failed=1, got %+v", sum)
	}
	for _, r := range parsed.Data.Results {
		if r.Email == "nocookie@x.com" {
			if !r.Skipped || !strings.Contains(r.Error, "Cookie") {
				t.Fatalf("无 Cookie 账号应 skipped: %+v", r)
			}
		} else if r.ID == "acc_not_exist" {
			if r.OK || !strings.Contains(r.Error, "不存在") {
				t.Fatalf("不存在账号应失败: %+v", r)
			}
		}
	}
	// 无 Cookie 账号不应被写入 RedeemInfo
	if a, _ := st.Get(parsed.Data.Results[0].ID); a != nil && a.RedeemedAt != "" {
		t.Fatal("skipped 账号不应写回兑换记录")
	}
}
