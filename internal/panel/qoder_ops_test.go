package panel

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/qoderops"
)

func TestQoderCheckinEndpoint(t *testing.T) {
	var gotUIDs []string
	var gotOnlyDaily bool
	p := New(Deps{
		QoderCheckin: func(ctx context.Context, uids []string, onlyDaily bool) ([]qoderops.CheckinOutcome, error) {
			gotUIDs, gotOnlyDaily = uids, onlyDaily
			return []qoderops.CheckinOutcome{
				{UID: "u1", Status: "ok", Earned: 100},
				{UID: "u2", Status: "skipped", Detail: "disabled"},
			}, nil
		},
	})
	code, out := doPanel(t, p, "POST", "/panel/api/qoder/checkin",
		`{"uids":["u1","u2"],"only_daily":true}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if !gotOnlyDaily || len(gotUIDs) != 2 {
		t.Fatalf("args uids=%v onlyDaily=%v", gotUIDs, gotOnlyDaily)
	}
	sum, _ := out["summary"].(map[string]any)
	if sum["ok"] != float64(1) || sum["skipped"] != float64(1) || sum["earned_credit"] != float64(100) {
		t.Fatalf("summary=%v", sum)
	}
}

func TestQoderCheckinBusy(t *testing.T) {
	p := New(Deps{
		QoderCheckin: func(ctx context.Context, uids []string, onlyDaily bool) ([]qoderops.CheckinOutcome, error) {
			return nil, qoderops.ErrBusy
		},
	})
	code, out := doPanel(t, p, "POST", "/panel/api/qoder/checkin", `{}`)
	if code != 409 {
		t.Fatalf("code=%d out=%v", code, out)
	}
}

func TestQoderClaimValidation(t *testing.T) {
	claimCalls := 0
	p := New(Deps{
		QoderClaim: func(ctx context.Context, uid, id string) (qoder.ClaimResult, error) {
			claimCalls++
			return qoder.ClaimResult{OK: true, Status: "CLAIMED", Amount: 100}, nil
		},
	})
	if code, _ := doPanel(t, p, "POST", "/panel/api/qoder/campaign/claim",
		`{"uid":"u1","campaign_id":"../etc"}`); code != 400 {
		t.Fatalf("非法 id 应 400，code=%d", code)
	}
	if claimCalls != 0 {
		t.Fatal("非法 id 不应调用上游")
	}
	code, out := doPanel(t, p, "POST", "/panel/api/qoder/campaign/claim",
		`{"uid":"u1","campaign_id":"c-1_2"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if claimCalls != 1 {
		t.Fatalf("claim calls=%d", claimCalls)
	}
}

func TestQoderPrefsAndActivity(t *testing.T) {
	var gotUID string
	var gotVal bool
	act := qoder.NewActivityLog(t.TempDir() + "/activity.jsonl")
	act.Log(qoder.ActivityEntry{UID: "u1", Nickname: "nick", Kind: "checkin", Status: "ok", Credits: 100})
	p := New(Deps{
		QoderSetCheckinPref: func(uid string, v bool) error {
			gotUID, gotVal = uid, v
			return nil
		},
		QoderActivity: act,
	})
	code, out := doPanel(t, p, "POST", "/panel/api/qoder/prefs",
		`{"uid":"u1","checkin_when_disabled":true}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("prefs code=%d out=%v", code, out)
	}
	if gotUID != "u1" || !gotVal {
		t.Fatalf("prefs args uid=%q val=%v", gotUID, gotVal)
	}
	code, out = doPanel(t, p, "GET", "/panel/api/qoder/activity?limit=10", "")
	if code != 200 || out["count"] != float64(1) {
		t.Fatalf("activity code=%d out=%v", code, out)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries=%v", entries)
	}
	first, _ := entries[0].(map[string]any)
	if first["uid"] != "u1" || first["kind"] != "checkin" {
		t.Fatalf("entry=%v", first)
	}
}

func TestQoderCampaignsRequiresUID(t *testing.T) {
	p := New(Deps{
		QoderCampaigns: func(ctx context.Context, uid string, force bool) (*qoder.CampaignsResult, error) {
			return &qoder.CampaignsResult{OK: true}, nil
		},
	})
	if code, _ := doPanel(t, p, "GET", "/panel/api/qoder/campaigns", ""); code != 400 {
		t.Fatalf("缺 uid 应 400，code=%d", code)
	}
}

func TestQoderBalanceIncludesLastCheckin(t *testing.T) {
	now := time.Now()
	p := New(Deps{
		QoderBalance: func(ctx context.Context, uids []string) ([]qoderops.BalanceOutcome, error) {
			credits := int64(42)
			return []qoderops.BalanceOutcome{{UID: "u1", Credits: &credits, Plan: "Pro"}}, nil
		},
		QoderLastCheckin: func() (time.Time, string) { return now, "total=1 ok=1" },
	})
	code, out := doPanel(t, p, "POST", "/panel/api/qoder/balance", `{}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["last_checkin_summary"] != "total=1 ok=1" {
		t.Fatalf("last_checkin=%v", out["last_checkin_summary"])
	}
	if _, ok := out["last_checkin_at"].(string); !ok {
		t.Fatalf("缺少 last_checkin_at: %v", out)
	}
}

func TestQoderOpsDisabled(t *testing.T) {
	p := New(Deps{})
	if code, _ := doPanel(t, p, "POST", "/panel/api/qoder/checkin", `{}`); code != 501 {
		t.Fatalf("未启用应 501，code=%d", code)
	}
	if code, _ := doPanel(t, p, "POST", "/panel/api/qoder/campaign/claim", `{}`); code != 501 {
		t.Fatalf("未启用应 501，code=%d", code)
	}
}
