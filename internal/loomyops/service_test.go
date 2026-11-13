package loomyops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
)

// fakeLoomy 模拟 Loomy 模型网关：first-login、新手任务列表/上报、积分、模型目录。
type fakeLoomy struct {
	mu        sync.Mutex
	checkedIn bool
	tasks     map[string]bool
	// claimAccepted 这些 key 上报成功但服务端不置位（模拟「报了但没领到」）。
	claimAccepted map[string]bool
	claimed       []string
	balance       int64
	taskReads     int
}

func newFakeLoomy() *fakeLoomy {
	return &fakeLoomy{tasks: map[string]bool{}, claimAccepted: map[string]bool{}, balance: 500}
}

func (f *fakeLoomy) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/points/first-login", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.checkedIn = true
		f.balance += 100
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{"dailyBalance": 100}})
	})
	mux.HandleFunc("/onboarding/tasks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.taskReads++
		tasks := map[string]any{}
		for k, v := range f.tasks {
			tasks[k] = v
		}
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{"tasks": tasks}})
	})
	mux.HandleFunc("/onboarding/tasks/complete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.claimed = append(f.claimed, body.Key)
		if f.claimAccepted[body.Key] {
			f.tasks[body.Key] = true
		}
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{"alreadyCompleted": false}})
	})
	mux.HandleFunc("/points/records", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{
			"balance": f.balance, "dailyBalance": 100, "availableBalance": f.balance,
		}})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testService(t *testing.T, fake *fakeLoomy) (*Service, *pool.Pool, string) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	a := auth.NewLoomyAuth("l-1", "测试号", "13800008000", "pwd", "sess", time.Now().Add(48*time.Hour).Unix())
	a.FilePath = auth.LoomyFilePath(dir, a.UID)
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save auth: %v", err)
	}
	p := pool.New("")
	p.Add(a)
	client := loomy.New(loomy.Config{ModelBase: srv.URL})
	svc := New(Config{
		Pool:            p,
		Client:          client,
		Gap:             time.Millisecond,
		TaskGap:         time.Millisecond,
		ClaimOnboarding: true,
	})
	return svc, p, dir
}

// TestCheckinClaimsOnboarding 首次签到：刷新日积分 + 领取全部未完成任务 + 写回余额/签到章。
func TestCheckinClaimsOnboarding(t *testing.T) {
	fake := newFakeLoomy()
	for _, task := range loomy.OnboardingTasks {
		fake.claimAccepted[task.Key] = true
	}
	svc, p, dir := testService(t, fake)

	out, err := svc.CheckinAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if len(out) != 1 || out[0].Status != "ok" {
		t.Fatalf("outcomes=%+v", out)
	}
	if !fake.checkedIn {
		t.Fatal("应调用 first-login")
	}
	if len(out[0].Claimed) != len(loomy.OnboardingTasks) {
		t.Fatalf("应确认领取 %d 项，得 %v", len(loomy.OnboardingTasks), out[0].Claimed)
	}
	if out[0].Earned != loomy.OnboardingTotalPoints {
		t.Fatalf("获得积分 %d want %d", out[0].Earned, loomy.OnboardingTotalPoints)
	}
	if out[0].Credits == nil || *out[0].Credits != 600 {
		t.Fatalf("余额未刷新: %+v", out[0].Credits)
	}
	if st, _ := p.Status("l-1"); st.Credits != 600 {
		t.Fatalf("池内余额 %d", st.Credits)
	}
	// 账号文件落盘：签到章 + 积分快照。
	auths, err := auth.LoadLoomyDir(dir)
	if err != nil {
		t.Fatalf("load auths: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("账号文件数 %d", len(auths))
	}
	if !auths[0].CheckedInToday(time.Now()) {
		t.Error("签到章未落盘")
	}
	if auths[0].Credits["available"] != float64(600) {
		t.Errorf("积分快照未落盘: %v", auths[0].Credits)
	}
}

// TestCheckinOnboardingPendingWhenNotConfirmed 上报成功但回读未置位 → 计入 pending，
// 不能当成已领取（这是新手任务领取最容易出现「报了但没分」的地方）。
func TestCheckinOnboardingPendingWhenNotConfirmed(t *testing.T) {
	fake := newFakeLoomy()
	// 什么都不接受：全部进 pending。
	svc, _, _ := testService(t, fake)
	out, err := svc.CheckinAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if len(out[0].Pending) != len(loomy.OnboardingTasks) {
		t.Fatalf("pending=%v", out[0].Pending)
	}
	if out[0].Earned != 0 {
		t.Fatalf("未确认不该计积分: %d", out[0].Earned)
	}
	if !strings.Contains(out[0].Detail, "回读未确认") {
		t.Fatalf("detail=%q", out[0].Detail)
	}
}

// TestCheckinIdempotentSameDay 当天第二次签到只刷新余额，不再打 first-login。
func TestCheckinIdempotentSameDay(t *testing.T) {
	fake := newFakeLoomy()
	svc, _, _ := testService(t, fake)
	if _, err := svc.CheckinAll(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.checkedIn = false // 重置探针
	fake.mu.Unlock()

	out, err := svc.CheckinAll(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Status != "already" {
		t.Fatalf("status=%q", out[0].Status)
	}
	if fake.checkedIn {
		t.Fatal("当天已签到不应再调 first-login")
	}
}

// TestCheckinSkipsDisabled 禁用的账号跳过（不消耗上游调用）。
func TestCheckinSkipsDisabled(t *testing.T) {
	fake := newFakeLoomy()
	svc, p, _ := testService(t, fake)
	p.Disable("l-1", "test")
	out, err := svc.CheckinAll(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Status != "skipped" || out[0].Detail != "disabled" {
		t.Fatalf("outcome=%+v", out[0])
	}
	if fake.checkedIn {
		t.Fatal("禁用账号不应打上游")
	}
}

// TestRefreshBalances 只读余额：拿到数字并写回池与账号文件。
func TestRefreshBalances(t *testing.T) {
	fake := newFakeLoomy()
	svc, p, _ := testService(t, fake)
	out, err := svc.RefreshBalances(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Credits == nil || *out[0].Credits != 500 {
		t.Fatalf("outcome=%+v", out[0])
	}
	if st, _ := p.Status("l-1"); st.Credits != 500 {
		t.Fatalf("池内余额 %d", st.Credits)
	}
}

// TestCheckinBusy 同一时刻只允许一次批次任务。
func TestCheckinBusy(t *testing.T) {
	fake := newFakeLoomy()
	svc, _, _ := testService(t, fake)
	done := make(chan struct{})
	go func() {
		svc.mu.Lock()
		close(done)
		time.Sleep(50 * time.Millisecond)
		svc.mu.Unlock()
	}()
	<-done
	if _, err := svc.CheckinAll(context.Background(), nil); err != ErrBusy {
		t.Fatalf("err=%v want ErrBusy", err)
	}
}

var _ = fmt.Sprintf

// TestCheckinClaimsOnboardingWhenAlreadyCheckedIn 当天已签到（补跑/换个时段再跑）也要把
// 还没领的新手任务领掉 —— 「自动任务」不能只在首次签到那一刻生效。
func TestCheckinClaimsOnboardingWhenAlreadyCheckedIn(t *testing.T) {
	fake := newFakeLoomy()
	for _, task := range loomy.OnboardingTasks {
		fake.claimAccepted[task.Key] = true
	}
	svc, p, dir := testService(t, fake)
	a := p.AuthByUID("l-1")
	a.LastCheckin = time.Now().Format(time.RFC3339)
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	out, err := svc.CheckinAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if out[0].Status != "already" {
		t.Fatalf("status=%q", out[0].Status)
	}
	if fake.checkedIn {
		t.Fatal("当天已签到不应再调 first-login")
	}
	if len(out[0].Claimed) != len(loomy.OnboardingTasks) {
		t.Fatalf("应补领 %d 项，得 %v", len(loomy.OnboardingTasks), out[0].Claimed)
	}
	// 全部确认领取后写死完成标记，下一趟不再空跑。
	auths, err := auth.LoadLoomyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || !auths[0].OnboardingComplete() {
		t.Fatalf("完成标记未落盘: %+v", auths)
	}
	reads := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.taskReads
	}
	before := reads()
	if _, err := svc.CheckinAll(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if reads() != before {
		t.Errorf("已全部领取后不应再读新手任务（%d → %d）", before, reads())
	}
}

// TestCheckinOnboardingNotMarkedDoneWhenClaimFails 上报失败的项不能被当成已完成，
// 否则完成标记会写死、账号永远不再补领。
func TestCheckinOnboardingNotMarkedDoneWhenClaimFails(t *testing.T) {
	fake := newFakeLoomy()
	svc, _, dir := testService(t, fake)
	if _, err := svc.CheckinAll(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	auths, err := auth.LoadLoomyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].OnboardingComplete() {
		t.Fatal("存在未确认任务时不该写完成标记")
	}
}
