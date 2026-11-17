package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHourSlotUnmarshal 时点解析：整数小时与 "H:MM" 字符串都接受，非法快速失败。
func TestHourSlotUnmarshal(t *testing.T) {
	cases := []struct {
		raw  string
		want HourSlot
	}{
		{`9`, 9 * 60},
		{`21`, 21 * 60},
		{`"10:30"`, 10*60 + 30},
		{`"09:00"`, 9 * 60},
		{`"23:59"`, 23*60 + 59},
		{`"10"`, 10 * 60},
	}
	for _, tc := range cases {
		var s HourSlot
		if err := jsonUnmarshal(tc.raw, &s); err != nil {
			t.Errorf("HourSlot(%s) 解析失败: %v", tc.raw, err)
			continue
		}
		if s != tc.want {
			t.Errorf("HourSlot(%s)=%d want %d", tc.raw, s, tc.want)
		}
	}

	bad := []string{`"abc"`, `"10:70"`, `"1:2:3"`, `1.5`}
	for _, raw := range bad {
		var s HourSlot
		if err := jsonUnmarshal(raw, &s); err == nil {
			t.Errorf("HourSlot(%s) 应报错", raw)
		}
	}
}

// jsonUnmarshal 是 encoding/json.Unmarshal 的薄封装（避免测试文件重复 import）。
func jsonUnmarshal(raw string, v any) error {
	return json.Unmarshal([]byte(raw), v)
}

// TestHourSlotString 时点展示格式。
func TestHourSlotString(t *testing.T) {
	if got := HourSlot(10*60 + 30).String(); got != "10:30" {
		t.Errorf("String()=%s want 10:30", got)
	}
	if got := HourSlot(9 * 60).String(); got != "09:00" {
		t.Errorf("String()=%s want 09:00", got)
	}
}

// TestDefaultScheduleActivityCount 缺省 ActivityReportCount=5（与 cmd/server Default() 对齐）。
// issue #49：cmd/activity 曾因复制结构体无默认值，缺省回落到 1，与 server 的 5 漂移。
func TestDefaultScheduleActivityCount(t *testing.T) {
	s := DefaultSchedule()
	if s.ActivityReportCount != 5 {
		t.Errorf("default ActivityReportCount=%d want 5", s.ActivityReportCount)
	}
	if !s.CheckinEnabled || !s.TravelEnabled || !s.ActivityEnabled || !s.KeepaliveEnabled {
		t.Errorf("all switches must default true: %+v", s)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9*60 || s.CheckinHours[1] != 21*60 {
		t.Errorf("checkin_hours=%v want [9:00 21:00]", s.CheckinHours)
	}
	if len(s.ActivityHours) != 1 || s.ActivityHours[0] != 10*60 {
		t.Errorf("activity_hours=%v want [10:00]", s.ActivityHours)
	}
	if len(s.SchoolHours) != 1 || s.SchoolHours[0] != 12*60 {
		t.Errorf("school_hours=%v want [12:00]", s.SchoolHours)
	}
	if len(s.CatHours) != 1 || s.CatHours[0] != 1*60 {
		t.Errorf("cat_hours=%v want [1:00]", s.CatHours)
	}
	if !s.SchoolEnabled || !s.CatEnabled {
		t.Errorf("school/cat switches must default true: %+v", s)
	}
}

// TestNormalizeScheduleThreeStates 缺省/显式 0/显式 N 三态默认值：
//   - 缺省（空 schedule）→ ActivityReportCount 保留 DefaultSchedule 的 5，hours 回落默认。
//   - 显式 0  → 归一为 1（兼容旧行为）。
//   - 显式 N  → 保留 N。
func TestNormalizeScheduleThreeStates(t *testing.T) {
	cases := []struct {
		name string
		s    Schedule
		want int
	}{
		{"absent_keeps_default", DefaultSchedule(), 5},
		{"explicit_zero_to_one", Schedule{ActivityReportCount: 0}, 1},
		{"explicit_negative_to_one", Schedule{ActivityReportCount: -3}, 1},
		{"explicit_n", Schedule{ActivityReportCount: 8}, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			if err := s.Normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if s.ActivityReportCount != tc.want {
				t.Errorf("ActivityReportCount=%d want %d", s.ActivityReportCount, tc.want)
			}
		})
	}
}

// TestNormalizeScheduleEmptyHoursFallback 空数组/缺省 hours 一律回落默认。
func TestNormalizeScheduleEmptyHoursFallback(t *testing.T) {
	s := Schedule{ActivityReportCount: 5} // hours 全零值（未配）
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9*60 || s.CheckinHours[1] != 21*60 {
		t.Errorf("checkin_hours=%v want [9:00 21:00]", s.CheckinHours)
	}
	if len(s.KeepaliveHours) != 1 || s.KeepaliveHours[0] != 22*60 {
		t.Errorf("keepalive_hours=%v want [22:00]", s.KeepaliveHours)
	}
	if len(s.TravelHours) != 2 || s.TravelHours[0] != 9*60 || s.TravelHours[1] != 21*60 {
		t.Errorf("travel_hours=%v want [9:00 21:00]", s.TravelHours)
	}
	if len(s.ActivityHours) != 1 || s.ActivityHours[0] != 10*60 {
		t.Errorf("activity_hours=%v want [10:00]", s.ActivityHours)
	}
	if len(s.SchoolHours) != 1 || s.SchoolHours[0] != 12*60 {
		t.Errorf("school_hours=%v want [12:00]", s.SchoolHours)
	}
	if len(s.CatHours) != 1 || s.CatHours[0] != 1*60 {
		t.Errorf("cat_hours=%v want [1:00]", s.CatHours)
	}
}

// TestNormalizeScheduleInvalidHour 非法时点快速失败并指向正确开关。
func TestNormalizeScheduleInvalidHour(t *testing.T) {
	cases := []struct {
		s           Schedule
		wantSwitch  string
	}{
		{Schedule{CheckinHours: []HourSlot{25 * 60}}, "checkin_enabled"},
		{Schedule{CheckinHours: []HourSlot{-1}}, "checkin_enabled"},
		{Schedule{KeepaliveHours: []HourSlot{24 * 60}}, "keepalive_enabled"},
		{Schedule{TravelHours: []HourSlot{-1}}, "travel_enabled"},
		{Schedule{ActivityHours: []HourSlot{24 * 60}}, "activity_enabled"},
		{Schedule{SchoolHours: []HourSlot{25 * 60}}, "school_enabled"},
		{Schedule{SchoolHours: []HourSlot{-1}}, "school_enabled"},
		{Schedule{CatHours: []HourSlot{24 * 60}}, "cat_enabled"},
		{Schedule{CatHours: []HourSlot{-1}}, "cat_enabled"},
	}
	for _, tc := range cases {
		err := tc.s.Normalize()
		if err == nil {
			t.Errorf("want error for %+v", tc.s)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error %q should point at schedule.%s", err.Error(), tc.wantSwitch)
		}
	}
}
