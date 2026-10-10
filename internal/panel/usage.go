// usage.go 面板「用量统计」页：按账号 / 小时 / 天 / 模型聚合积分消耗。
//
// 数据源是上游的「积分消耗明细」（get-user-request-usage，与官网同源），
// 由 cmd/server 注入的 Deps.QueryUsage 拉取——panel 包不感知 upstream/pool 细节。
//
// 关键口径：明细里**免费请求（credit=0）也逐条记录**，所以本页同时反映
// 「调用量」与「扣费」。两者分开展示：免费模型跑得再多，credit 也是 0。
package panel

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// usageLoc 上游明细的时间口径。requestTime 是 UTC+8 墙钟（与官网展示一致），
// 分桶必须按同一时区切，否则「今天/本小时」会在 UTC 边界上错位。
var usageLoc = time.FixedZone("UTC+8", 8*60*60)

// UsageLoc 导出明细时区，供 cmd/server 按同一口径解析 requestTime。
func UsageLoc() *time.Location { return usageLoc }

// UsageRecord 单条积分消耗明细（cmd/server 从上游拉取后注入）。
type UsageRecord struct {
	UID      string
	Nickname string
	Realm    string
	Time     time.Time // 已按 usageLoc 解析
	Credit   float64
	Model    string
}

// usageCacheTTL 明细缓存时长。
//
// 为什么要缓存：一次刷新要对**每个账号**各发一轮上游请求（当前 11 个号 = 11 个请求，
// 且分页时更多）。上游 billing 域有风控，而明细本身是天粒度数据，没必要每次打开
// 页面都重拉。TTL 内直接吃缓存，用户可点「强制刷新」绕过。
const usageCacheTTL = 15 * time.Minute

// usageHourWindow / usageDayWindow 展示窗口：近 24 小时、近 7 天。
const (
	usageHourWindow = 24
	usageDayWindow  = 7
)

// usageBucket 一个时间桶（小时或天）的聚合。
type usageBucket struct {
	Key    string  `json:"key"`   // 排序/对齐用（"2006-01-02 15" 或 "2006-01-02"）
	Label  string  `json:"label"` // 展示用（"15:00" 或 "10-09"）
	Calls  int     `json:"calls"`
	Credit float64 `json:"credit"`
}

// usageModelRow 按模型的聚合。
type usageModelRow struct {
	Model  string  `json:"model"`
	Calls  int     `json:"calls"`
	Credit float64 `json:"credit"`
}

// usageAccountRow 按账号的聚合（窗口内总量 + 今日）。
type usageAccountRow struct {
	UID         string  `json:"uid"`
	Nickname    string  `json:"nickname"`
	Realm       string  `json:"realm"`
	Calls       int     `json:"calls"`
	Credit      float64 `json:"credit"`
	CallsToday  int     `json:"calls_today"`
	CreditToday float64 `json:"credit_today"`
	FreeCalls   int     `json:"free_calls"` // credit=0 的调用数（免费档）
}

// usageAccountDetail 单账号的细分（供前端点击账号下钻）。
type usageAccountDetail struct {
	Hourly []usageBucket   `json:"hourly"`
	Daily  []usageBucket   `json:"daily"`
	Models []usageModelRow `json:"models"`
}

// usageError 拉取失败的账号。
type UsageError struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Realm    string `json:"realm"`
	Error    string `json:"error"`
}

// usageAgg 聚合结果。
type usageAgg struct {
	GeneratedAt time.Time                     `json:"generated_at"`
	Windows     map[string]int                `json:"windows"`
	Accounts    []usageAccountRow             `json:"accounts"`
	Hourly      []usageBucket                 `json:"hourly"`
	Daily       []usageBucket                 `json:"daily"`
	Models      []usageModelRow               `json:"models"`
	Detail      map[string]usageAccountDetail `json:"detail"`
	Errors      []UsageError                  `json:"errors"`
	TotalCalls  int                           `json:"total_calls"`
	TotalCredit float64                       `json:"total_credit"`
}

// usageCache 明细聚合结果的进程内缓存。
type usageCache struct {
	mu      sync.Mutex
	at      time.Time
	payload *usageAgg
}

// handleUsage 返回用量统计。默认吃缓存；?refresh=1 强制重拉上游。
func (p *Panel) handleUsage(w http.ResponseWriter, r *http.Request) {
	if p.deps.QueryUsage == nil {
		errJSON(w, http.StatusNotImplemented, "用量统计未接线（QueryUsage 为空）")
		return
	}
	force := r.URL.Query().Get("refresh") == "1"

	p.usage.mu.Lock()
	defer p.usage.mu.Unlock()

	now := time.Now()
	if !force && p.usage.payload != nil && now.Sub(p.usage.at) < usageCacheTTL {
		out := *p.usage.payload
		out.GeneratedAt = p.usage.at
		okJSON(w, map[string]any{
			"ok":           true,
			"cached":       true,
			"cache_age_ms": now.Sub(p.usage.at).Milliseconds(),
			"ttl_ms":       usageCacheTTL.Milliseconds(),
			"data":         out,
		})
		return
	}

	// 一次拉取覆盖整个展示窗口（7 天），小时/天桶都在本地切分，
	// 避免为了两个窗口各打一轮上游。
	since := startOfDay(now).AddDate(0, 0, -(usageDayWindow - 1))
	recs, errs, err := p.deps.QueryUsage(since)
	if err != nil {
		// 拉取整体失败：有旧缓存就降级返回（标注 stale），否则报错。
		if p.usage.payload != nil {
			okJSON(w, map[string]any{
				"ok":     true,
				"cached": true,
				"stale":  true,
				"error":  err.Error(),
				"data":   p.usage.payload,
			})
			return
		}
		errJSON(w, http.StatusBadGateway, "拉取用量明细失败："+err.Error())
		return
	}

	agg := aggregateUsage(recs, errs, now)
	p.usage.at = now
	p.usage.payload = agg

	okJSON(w, map[string]any{
		"ok":           true,
		"cached":       false,
		"cache_age_ms": 0,
		"ttl_ms":       usageCacheTTL.Milliseconds(),
		"data":         agg,
	})
}

// startOfDay 返回 t 所在自然日 00:00（UTC+8）。
func startOfDay(t time.Time) time.Time {
	l := t.In(usageLoc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, usageLoc)
}

// aggregateUsage 把逐条明细切成 账号 / 小时 / 天 / 模型 四组视图。
func aggregateUsage(recs []UsageRecord, errs []UsageError, now time.Time) *usageAgg {
	today := startOfDay(now)
	hourStart := now.In(usageLoc).Truncate(time.Hour).Add(-(usageHourWindow - 1) * time.Hour)
	dayStart := today.AddDate(0, 0, -(usageDayWindow - 1))

	// 预置空桶：即使某小时/某天没有请求，也要在图上占位（否则曲线会"跳变"）。
	hourlyIdx := map[string]int{}
	hourly := make([]usageBucket, 0, usageHourWindow)
	for i := 0; i < usageHourWindow; i++ {
		h := hourStart.Add(time.Duration(i) * time.Hour)
		hourlyIdx[h.Format("2006-01-02 15")] = len(hourly)
		hourly = append(hourly, usageBucket{Key: h.Format("2006-01-02 15"), Label: h.Format("15:04")})
	}
	dailyIdx := map[string]int{}
	daily := make([]usageBucket, 0, usageDayWindow)
	for i := 0; i < usageDayWindow; i++ {
		d := dayStart.AddDate(0, 0, i)
		dailyIdx[d.Format("2006-01-02")] = len(daily)
		daily = append(daily, usageBucket{Key: d.Format("2006-01-02"), Label: d.Format("01-02")})
	}

	acctIdx := map[string]int{}
	var accounts []usageAccountRow
	modelsIdx := map[string]int{}
	var models []usageModelRow

	// 下钻明细：按 uid 存各自的小时/天/模型桶。
	type detailAcc struct {
		hourly []usageBucket
		daily  []usageBucket
		models []usageModelRow
		hIdx   map[string]int
		dIdx   map[string]int
		mIdx   map[string]int
	}
	details := map[string]*detailAcc{}
	newDetail := func() *detailAcc {
		d := &detailAcc{hIdx: map[string]int{}, dIdx: map[string]int{}, mIdx: map[string]int{}}
		d.hourly = append(d.hourly, hourly...) // 复制空桶模板
		d.daily = append(d.daily, daily...)
		return d
	}

	var totalCalls int
	var totalCredit float64

	for _, rec := range recs {
		lt := rec.Time.In(usageLoc)

		// ── 账号维度 ──
		ai, ok := acctIdx[rec.UID]
		if !ok {
			ai = len(accounts)
			acctIdx[rec.UID] = ai
			accounts = append(accounts, usageAccountRow{
				UID: rec.UID, Nickname: rec.Nickname, Realm: rec.Realm,
			})
		}
		accounts[ai].Calls++
		accounts[ai].Credit += rec.Credit
		if rec.Credit == 0 {
			accounts[ai].FreeCalls++
		}
		if !lt.Before(today) {
			accounts[ai].CallsToday++
			accounts[ai].CreditToday += rec.Credit
		}

		// ── 模型维度 ──
		m := rec.Model
		if m == "" {
			m = "（未知）"
		}
		mi, ok := modelsIdx[m]
		if !ok {
			mi = len(models)
			modelsIdx[m] = mi
			models = append(models, usageModelRow{Model: m})
		}
		models[mi].Calls++
		models[mi].Credit += rec.Credit

		// ── 时间维度（全局桶 + 该账号自己的桶）──
		da, ok := details[rec.UID]
		if !ok {
			da = newDetail()
			details[rec.UID] = da
		}
		hk := lt.Format("2006-01-02 15")
		if i, ok := hourlyIdx[hk]; ok {
			hourly[i].Calls++
			hourly[i].Credit += rec.Credit
		}
		if i, ok := da.hIdx[hk]; ok {
			da.hourly[i].Calls++
			da.hourly[i].Credit += rec.Credit
		}
		dk := lt.Format("2006-01-02")
		if i, ok := dailyIdx[dk]; ok {
			daily[i].Calls++
			daily[i].Credit += rec.Credit
		}
		if i, ok := da.dIdx[dk]; ok {
			da.daily[i].Calls++
			da.daily[i].Credit += rec.Credit
		}
		if i, ok := da.mIdx[m]; !ok {
			da.mIdx[m] = len(da.models)
			da.models = append(da.models, usageModelRow{Model: m, Calls: 1, Credit: rec.Credit})
		} else {
			da.models[i].Calls++
			da.models[i].Credit += rec.Credit
		}

		totalCalls++
		totalCredit += rec.Credit
	}

	// 账号按消耗降序（消耗相同看调用量），前端首屏就是最该关注的号。
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Credit != accounts[j].Credit {
			return accounts[i].Credit > accounts[j].Credit
		}
		return accounts[i].Calls > accounts[j].Calls
	})
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Credit != models[j].Credit {
			return models[i].Credit > models[j].Credit
		}
		return models[i].Calls > models[j].Calls
	})

	detail := make(map[string]usageAccountDetail, len(details))
	for uid, d := range details {
		sort.SliceStable(d.models, func(i, j int) bool {
			if d.models[i].Credit != d.models[j].Credit {
				return d.models[i].Credit > d.models[j].Credit
			}
			return d.models[i].Calls > d.models[j].Calls
		})
		detail[uid] = usageAccountDetail{Hourly: d.hourly, Daily: d.daily, Models: d.models}
	}

	if errs == nil {
		errs = []UsageError{}
	}
	return &usageAgg{
		GeneratedAt: now,
		Windows:     map[string]int{"hours": usageHourWindow, "days": usageDayWindow},
		Accounts:    accounts,
		Hourly:      hourly,
		Daily:       daily,
		Models:      models,
		Detail:      detail,
		Errors:      errs,
		TotalCalls:  totalCalls,
		TotalCredit: round2(totalCredit),
	}
}

// round2 把 credit 归一到 2 位小数，消除浮点累加的尾差（0.30000000000000004）。
func round2(v float64) float64 {
	return float64(int64(v*100+0.5*sign(v))) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
