package qoderops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
)

func newOpsFixture(t *testing.T, h http.Handler) (*Service, *pool.Pool, *auth.Auth, *qoder.ActivityLog) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := qoder.New(qoder.Config{Endpoints: map[string]qoder.Endpoint{
		"cn": {OpenAPI: srv.URL, Website: srv.URL, UA: "QoderWork/test"},
	}})
	dir := t.TempDir()
	a := auth.NewQoderAuth("u1", "nick", "at-1", "rt-1", "", "cn", "test")
	a.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	a.FilePath = filepath.Join(dir, "qoder-u1.json")
	qp := pool.New(filepath.Join(dir, "qoder_state.json"))
	t.Cleanup(qp.Close)
	qp.SyncToDir([]*auth.Auth{a})
	act := qoder.NewActivityLog(filepath.Join(dir, "activity.jsonl"))
	svc := New(Config{Pool: qp, Client: client, Activity: act, Gap: time.Millisecond})
	return svc, qp, a, act
}

func campaignServer(claimHits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns":
			_, _ = w.Write([]byte(`{"showCampaign":true,"claimable":true,"campaigns":[
				{"campaignId":"c1","campaignKey":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
				 "benefit":{"kind":"CREDITS","amount":100},
				 "placements":[{"content":{"zh":{"title":"每日领取 100 Credits"}}}]}]}`))
		case "/sash/api/v1/me/campaigns/c1/claim":
			if claimHits != nil {
				claimHits.Add(1)
			}
			_, _ = w.Write([]byte(`{"status":"CLAIMED","benefit":{"amount":100}}`))
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userQuota":{"remaining":900,"used":100,"total":1000},
				"addOnQuota":{"remaining":100,"used":0,"total":100}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"planName":"Pro"}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func TestCheckinAllClaimsAndUpdatesPool(t *testing.T) {
	var hits atomic.Int32
	svc, qp, a, act := newOpsFixture(t, campaignServer(&hits))
	out, err := svc.CheckinAll(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(out) != 1 || out[0].Status != "ok" || out[0].Earned != 100 {
		t.Fatalf("outcome=%+v", out)
	}
	if hits.Load() != 1 {
		t.Fatalf("claim hits=%d", hits.Load())
	}
	if st, ok := qp.Status("u1"); !ok || st.Credits != 1000 {
		t.Fatalf("pool status=%+v ok=%v", st, ok)
	}
	if a.QoderStateSnapshot().Plan != "Pro" {
		t.Fatal("套餐未写回")
	}
	entries := act.Tail(10)
	if len(entries) != 1 || entries[0].Kind != "checkin" || entries[0].Status != "ok" {
		t.Fatalf("activity=%+v", entries)
	}
	if at, _ := svc.LastCheckin(); at.IsZero() {
		t.Fatal("LastCheckin 未记录")
	}
}

func TestCheckinAllSkipsDisabledUnlessPrefSet(t *testing.T) {
	var hits atomic.Int32
	svc, qp, a, _ := newOpsFixture(t, campaignServer(&hits))
	qp.Disable("u1", "test")
	out, err := svc.CheckinAll(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if out[0].Status != "skipped" || out[0].Detail != "disabled" {
		t.Fatalf("disabled outcome=%+v", out[0])
	}
	if hits.Load() != 0 {
		t.Fatalf("disabled 账号不应请求上游，hits=%d", hits.Load())
	}
	if err := svc.SetCheckinWhenDisabled("u1", true); err != nil {
		t.Fatalf("SetCheckinWhenDisabled: %v", err)
	}
	out, err = svc.CheckinAll(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("CheckinAll(2): %v", err)
	}
	if out[0].Status == "skipped" {
		t.Fatalf("checkinWhenDisabled 后仍被跳过：%+v", out[0])
	}
	if !a.QoderStateSnapshot().CheckinWhenDisabled {
		t.Fatal("偏好未写回账号状态")
	}
}

func TestCheckinAllUIDFilter(t *testing.T) {
	var hits atomic.Int32
	svc, _, _, _ := newOpsFixture(t, campaignServer(&hits))
	out, err := svc.CheckinAll(context.Background(), []string{"other"}, false)
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(out) != 0 || hits.Load() != 0 {
		t.Fatalf("UID 过滤失效：out=%+v hits=%d", out, hits.Load())
	}
}

func TestRefreshBalances(t *testing.T) {
	svc, qp, _, _ := newOpsFixture(t, campaignServer(nil))
	out, err := svc.RefreshBalances(context.Background(), nil)
	if err != nil {
		t.Fatalf("RefreshBalances: %v", err)
	}
	if len(out) != 1 || out[0].Error != "" || out[0].Credits == nil || *out[0].Credits != 1000 {
		t.Fatalf("outcome=%+v", out)
	}
	if out[0].Plan != "Pro" {
		t.Fatalf("plan=%q", out[0].Plan)
	}
	if out[0].Detail["remain_effective"] != int64(1000) {
		t.Fatalf("detail=%v", out[0].Detail)
	}
	if st, _ := qp.Status("u1"); st.Credits != 1000 {
		t.Fatalf("pool credits=%d", st.Credits)
	}
}

func TestClaimPersistsCode(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns/c9/claim":
			_, _ = w.Write([]byte(`{"status":"CLAIMED","redemptionCode":"MILK-1","benefit":{"amount":0}}`))
		default:
			http.NotFound(w, r)
		}
	})
	svc, _, a, act := newOpsFixture(t, h)
	res, err := svc.Claim(context.Background(), "u1", "c9")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !res.OK || res.RedemptionCode != "MILK-1" {
		t.Fatalf("claim=%+v", res)
	}
	if got := a.QoderStateSnapshot().CampaignCodes["c9"]; got != "MILK-1" {
		t.Fatalf("code=%q", got)
	}
	if entries := act.Tail(5); len(entries) != 1 || entries[0].Kind != "claim" {
		t.Fatalf("activity=%+v", entries)
	}
}
