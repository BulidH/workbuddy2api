package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func newOpsTestClient(t *testing.T, h http.Handler) (*Client, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(Config{Endpoints: map[string]Endpoint{
		"cn": {OpenAPI: srv.URL, Website: srv.URL, UA: "QoderWork/test"},
	}})
	a := auth.NewQoderAuth("u1", "nick", "at-1", "rt-1", "", "cn", "test")
	a.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	a.FilePath = t.TempDir() + "/qoder-u1.json"
	return c, a
}

func TestFetchCreditsNormalizes(t *testing.T) {
	var planHits atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
			t.Errorf("authorization=%q", got)
		}
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userQuota":{"remaining":1000,"used":200,"total":1200},
				"addOnQuota":{"remaining":300,"used":100,"total":400},
				"isQuotaExceeded":false,"totalUsagePercentage":25}`))
		case "/api/v2/user/plan":
			planHits.Add(1)
			_, _ = w.Write([]byte(`{"planName":"Pro Trial"}`))
		default:
			http.NotFound(w, r)
		}
	})
	c, a := newOpsTestClient(t, h)
	cr, err := c.FetchCredits(context.Background(), a)
	if err != nil {
		t.Fatalf("FetchCredits: %v", err)
	}
	if cr.Remain != 1300 || cr.RemainEffective != 1300 || cr.Used != 300 || cr.Size != 1600 {
		t.Fatalf("credits=%+v", cr)
	}
	if len(cr.Packages) != 2 {
		t.Fatalf("packages=%d want 2", len(cr.Packages))
	}
	st := a.QoderStateSnapshot()
	if creditsRemainMap(st.Credits) != 1300 {
		t.Fatalf("persisted credits=%v", st.Credits)
	}
	_ = a.UpdateQoderState(func(s *auth.QoderState) { s.LastCreditsDelta = 0 }) // 确保文件可写
	if got := c.FetchPlan(context.Background(), a); got != "Pro Trial" {
		t.Fatalf("plan=%q", got)
	}
	if planHits.Load() != 1 {
		t.Fatalf("plan hits=%d", planHits.Load())
	}
	if a.QoderStateSnapshot().Plan != "Pro Trial" {
		t.Fatal("plan 未写回账号状态")
	}
}

func TestFetchCreditsRejectsDegenerateBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"isQuotaExceeded":true}`))
	})
	c, a := newOpsTestClient(t, h)
	if _, err := c.FetchCredits(context.Background(), a); err == nil {
		t.Fatal("退化体应报错")
	}
	if a.QoderStateSnapshot().Credits != nil {
		t.Fatal("退化体不应覆盖旧快照")
	}
}

func TestCheckinStatusAndClaim(t *testing.T) {
	var claimed atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/daily-check-in/status":
			_, _ = w.Write([]byte(`{"status":"CLAIMABLE","currentStreakDays":3,"rewardCredits":100}`))
		case "/sash/api/v1/me/daily-check-in/claim":
			claimed.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"rewardCredits":100}`))
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userQuota":{"remaining":500,"used":100,"total":600},"addOnQuota":{"remaining":0,"used":0,"total":0}}`))
		default:
			http.NotFound(w, r)
		}
	})
	c, a := newOpsTestClient(t, h)
	res := c.Checkin(context.Background(), a)
	if !res.OK || res.RewardCredits != 100 {
		t.Fatalf("checkin=%+v", res)
	}
	if claimed.Load() != 1 {
		t.Fatalf("claim hits=%d", claimed.Load())
	}
	if !a.CheckedInToday(time.Now()) {
		t.Fatal("lastCheckin 未盖章")
	}
	// 第二次：状态改为 CLAIMED，应走 already 且不再 POST。
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("already 状态不应再 POST")
		}
		_, _ = w.Write([]byte(`{"status":"CLAIMED","lastClaimedAt":` +
			itoa(time.Now().In(utc8).Unix()) + `}`))
	})
	c2, a2 := newOpsTestClient(t, h2)
	res2 := c2.Checkin(context.Background(), a2)
	if !res2.OK || !res2.Already {
		t.Fatalf("second checkin=%+v", res2)
	}
}

func TestCampaignCheckinClaimsCredits(t *testing.T) {
	var claimHits atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("cosy-clienttype"); got != "10" {
			t.Errorf("cosy-clienttype=%q", got)
		}
		if got := r.Header.Get("cosy-version"); got == "" {
			t.Error("缺少 cosy-version")
		}
		if strings.HasSuffix(r.URL.Path, "/claim") && r.Method == http.MethodGet {
			t.Errorf("claim 必须 POST")
		}
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns":
			_, _ = w.Write([]byte(`{"showCampaign":true,"claimable":true,"campaignUrl":"https://qoder.com.cn/activities",
				"campaigns":[{"campaignId":"c1","campaignKey":"daily","actionType":"CLAIM_BENEFIT",
				"claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100},
				"placements":[{"content":{"zh":{"title":"每日领取 100 Credits","description":"d","detailUrl":"https://x"}}}]}]}`))
		case "/sash/api/v1/me/campaigns/c1/claim":
			claimHits.Add(1)
			_, _ = w.Write([]byte(`{"status":"CLAIMED","benefit":{"amount":100}}`))
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userQuota":{"remaining":900,"used":0,"total":900},"addOnQuota":{"remaining":100,"used":0,"total":100}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"planName":"Pro"}`))
		default:
			http.NotFound(w, r)
		}
	})
	c, a := newOpsTestClient(t, h)
	out := c.CampaignCheckin(context.Background(), a, 0, []string{"", "CREDITS"})
	if !out.OK || len(out.Claimed) != 1 || out.Earned != 100 {
		t.Fatalf("outcome=%+v", out)
	}
	if claimHits.Load() != 1 {
		t.Fatalf("claim hits=%d", claimHits.Load())
	}
	if out.NextAvailable <= time.Now().Unix() {
		t.Fatalf("next_available_at=%d", out.NextAvailable)
	}
	if !a.CheckedInToday(time.Now()) {
		t.Fatal("活动领取后应盖章签到")
	}
	if got := a.QoderStateSnapshot().CampaignCodes; len(got) != 0 {
		t.Fatalf("campaign codes=%v", got)
	}
}

func TestCampaignClaimSavesRedemptionCode(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns/c9/claim":
			_, _ = w.Write([]byte(`{"status":"CLAIMED","redemptionCode":"MILK-TEA-1","benefit":{"amount":0}}`))
		default:
			http.NotFound(w, r)
		}
	})
	c, a := newOpsTestClient(t, h)
	res := c.ClaimCampaign(context.Background(), a, "c9")
	if !res.OK || res.RedemptionCode != "MILK-TEA-1" {
		t.Fatalf("claim=%+v", res)
	}
	if got := a.QoderStateSnapshot().CampaignCodes["c9"]; got != "MILK-TEA-1" {
		t.Fatalf("persisted code=%q", got)
	}
}

func TestCampaignBlockedCooldown(t *testing.T) {
	var claimHits atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns":
			_, _ = w.Write([]byte(`{"showCampaign":true,"claimable":true,"campaigns":[
				{"campaignId":"c1","campaignKey":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
				 "benefit":{"kind":"CREDITS","amount":100}}]}`))
		case "/sash/api/v1/me/campaigns/c1/claim":
			claimHits.Add(1)
			_, _ = w.Write([]byte(`{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`))
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userQuota":{"remaining":1,"used":0,"total":1},"addOnQuota":{"remaining":0,"used":0,"total":0}}`))
		default:
			http.NotFound(w, r)
		}
	})
	c, a := newOpsTestClient(t, h)
	out := c.CampaignCheckin(context.Background(), a, 0, nil)
	if len(out.Blocked) != 1 {
		t.Fatalf("blocked=%+v", out)
	}
	if got := a.QoderStateSnapshot().CampaignBlockedUntil["c1"]; got <= time.Now().Unix() {
		t.Fatalf("blocked_until=%d", got)
	}
	if !a.CheckedInToday(time.Now()) {
		t.Fatal("同人已领取也应盖章，避免调度器反复打上游")
	}
	// 冷却期内第二轮不再 POST。
	out2 := c.CampaignCheckin(context.Background(), a, 0, nil)
	if claimHits.Load() != 1 {
		t.Fatalf("冷却期内不应重复 claim，hits=%d", claimHits.Load())
	}
	if len(out2.Blocked) == 0 {
		t.Fatal("冷却期应给出 blocked 结论")
	}
}

func TestCheckinUnavailableReasons(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	c, a := newOpsTestClient(t, h)
	res := c.Checkin(context.Background(), a)
	if !res.OK || !res.Unavailable {
		t.Fatalf("404 应归一为 unavailable：%+v", res)
	}
}

func TestNormalizeQuotaComposition(t *testing.T) {
	body := `{"userQuota":{"remaining":100,"used":0,"total":100},
		"addOnQuota":{"remaining":0,"used":0,"total":0},
		"dedicatedResourcePackages":[{"remaining":2000,"used":0,"total":2000,
			"displayLabels":[{"dimension":"title","valueI18n":{"zh-CN":"SOTA 专属积分"}}]}],
		"orgResourcePackage":{"remaining":952,"used":1548,"cap":2500,"available":true}}`
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	cr, err := normalizeQuota(m, body)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cr.Remain != 2100 || cr.Size != 2100 {
		t.Fatalf("remain=%d size=%d want 2100（SOTA 计入、共享池单列）", cr.Remain, cr.Size)
	}
	if cr.Shared == nil || cr.Shared.Remain != 952 {
		t.Fatalf("shared=%+v", cr.Shared)
	}
	if cr.RemainEffective != 2100+952 {
		t.Fatalf("effective=%d", cr.RemainEffective)
	}
	if len(cr.Composition) < 3 {
		t.Fatalf("composition=%+v", cr.Composition)
	}
}

func TestParseVPCEndpoint(t *testing.T) {
	ep, err := ParseVPCEndpoint("acme")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ep.OpenAPI != "https://acme-openapi.vpc.qoder.com.cn" || ep.Domain != "acme.vpc.qoder.com.cn" {
		t.Fatalf("endpoint=%+v", ep)
	}
	if _, err := ParseVPCEndpoint("evil.example.com"); err == nil {
		t.Fatal("非 VPC 域应拒绝")
	}
	if _, err := ParseVPCEndpoint("acme-openapi.vpc.qoder.com.cn:8443"); err == nil {
		t.Fatal("端口应拒绝")
	}
	if _, err := ParseVPCEndpoint("acme.vpc.qoder.com.cn/path"); err == nil {
		t.Fatal("路径应拒绝")
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
