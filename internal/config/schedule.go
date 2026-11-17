// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HourSlot 排程时点：自当日 0 点的分钟数（9:00 → 540，10:30 → 630）。
//
// JSON 里两种写法都接受，便于配置：
//   - 整数小时：9（等价 "09:00"）
//   - 字符串：  "10:30"（签到窗口 10:00 开放后，10:30 补跑一趟）
//
// 为什么用自定义类型而不是直接 []int：上游活动平台的每日领取窗口是 10:00 开放，
// 整点排程在 9:00 那趟跑时活动还没上线，只能等下一趟，用户感知就是「没自动签到」。
// 支持分钟粒度后可以把时点精确卡在窗口开放之后。
type HourSlot int

// UnmarshalJSON 兼容整数小时与 "H:MM" 字符串两种写法。
//
// 只做格式解析，范围校验留给 Normalize/validateHours——那边报错能指向正确的
// 禁用开关（checkin_enabled 等），这里提前拒绝会丢掉这层指引。
func (s *HourSlot) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, "\"") {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		mins, err := parseSlot(str)
		if err != nil {
			return err
		}
		*s = HourSlot(mins)
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("排程时点 %s 非法：应为整数小时或 \"H:MM\" 字符串", raw)
	}
	*s = HourSlot(n * 60)
	return nil
}

// parseSlot 解析 "H" / "H:MM" / "HH:MM" 为分钟数。
// 分钟范围（0-59）在此校验（格式级错误，直接拒）；小时范围留给 Normalize 校验，
// 那边报错能指向正确的禁用开关。
func parseSlot(str string) (int, error) {
	parts := strings.Split(str, ":")
	if len(parts) == 1 {
		n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, fmt.Errorf("排程时点 %q 非法：应为整数小时或 \"H:MM\"", str)
		}
		return n * 60, nil
	}
	if len(parts) != 2 {
		return 0, fmt.Errorf("排程时点 %q 非法：应为 \"H:MM\"", str)
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("排程时点 %q 非法：应为 \"H:MM\"", str)
	}
	if m < 0 || m > 59 {
		return 0, fmt.Errorf("排程时点 %q 非法：分钟须在 0-59", str)
	}
	return h*60 + m, nil
}

// String 输出 "H:MM" 便于日志与错误信息展示。
func (s HourSlot) String() string {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(s) * time.Minute).Format("15:04")
}

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 六类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	CheckinHours   []HourSlot `json:"checkin_hours"`   // [9:00,21:00]
	TravelHours    []HourSlot `json:"travel_hours"`    // [9:00,21:00]
	ActivityHours  []HourSlot `json:"activity_hours"`  // [10:00]
	KeepaliveHours []HourSlot `json:"keepalive_hours"` // [22:00]
	SchoolHours    []HourSlot `json:"school_hours"`    // [12:00] 开学季任务（迁移自 school/cat 两条系统 crontab）
	CatHours       []HourSlot `json:"cat_hours"`       // [1:00] 夜猫窗口 23-08 CST，01:00 窗口内补 1 次
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/SchoolEnabled/CatEnabled
	// 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
	TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
	SchoolEnabled    bool `json:"school_enabled"`    // 缺省 true；false = 停开学季任务
	CatEnabled       bool `json:"cat_enabled"`       // 缺省 true；false = 停夜猫子任务
	// CheckinOnStartup 启动后补跑一次签到（缺省 true）。
	//
	// 为什么需要：调度器只在 nextFire 算出的整点触发、**启动时不跑**，而 nextFire
	// 对已过去的时点是直接 +24h 排到明天，没有任何补偿。容器若在签到时点（如 10:00）
	// 未运行——重启、停机、改配置——当天那次签到就被整体跳过，用户看到的现象就是
	// 「自动签到好像没生效」。签到幂等（当天重复签到上游返回 already），补跑安全，
	// 代价只是多一轮余额查询。
	CheckinOnStartup bool `json:"checkin_on_startup"`
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`
	// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
	// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []HourSlot{9 * 60, 21 * 60},
		TravelHours:         []HourSlot{9 * 60, 21 * 60},
		ActivityHours:       []HourSlot{10 * 60},
		KeepaliveHours:      []HourSlot{22 * 60},
		SchoolHours:         []HourSlot{12 * 60},
		CatHours:            []HourSlot{1 * 60},
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		SchoolEnabled:       true,
		CatEnabled:          true,
		CheckinOnStartup:    true, // 启动补跑签到：容器在签到时点未运行时兜底
		ActivityReportCount: 5,    // 领猫前置需 5 次对话，5 连发刷满 chat_5
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，ActivityReportCount 归一，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），在此补齐。
// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
//
// ActivityReportCount：0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
// 注意这是「显式配 0 = 旧行为」的兼容语义，与 scheduler.New 的 <=0 → 1 归一一致；
// 「缺省 = 5」由 DefaultSchedule 在 Unmarshal 前置入，是另一条路径，两者不合并。
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []HourSlot{9 * 60, 21 * 60}
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = []HourSlot{9 * 60, 21 * 60}
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = []HourSlot{10 * 60}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []HourSlot{22 * 60}
	}
	if len(s.SchoolHours) == 0 {
		s.SchoolHours = []HourSlot{12 * 60}
	}
	if len(s.CatHours) == 0 {
		s.CatHours = []HourSlot{1 * 60}
	}
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	return s.validateHours()
}

// validateHours 校验排程时点落在 0:00-23:59。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法时点被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个时点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.school_hours", "school_enabled", s.SchoolHours); err != nil {
		return err
	}
	return checkHourRange("schedule.cat_hours", "cat_enabled", s.CatHours)
}

func checkHourRange(field, switchKey string, hours []HourSlot) error {
	for _, h := range hours {
		if h < 0 || h >= 24*60 {
			return fmt.Errorf("%s: %s 不是合法时点（0:00-23:59）；如要关闭该任务请设 schedule.%s=false", field, h.String(), switchKey)
		}
	}
	return nil
}
