package scheduler

import (
	"testing"
	"time"

	"workbuddy2api/internal/qoderops"
)

func qoderOnlyScheduler(t *testing.T, cfg Config) *Scheduler {
	t.Helper()
	cfg.CheckinDisabled = true
	cfg.TravelDisabled = true
	cfg.ActivityDisabled = true
	cfg.KeepaliveDisabled = true
	cfg.SchoolDisabled = true
	cfg.CatDisabled = true
	cfg.QoderOps = qoderops.New(qoderops.Config{})
	return New(cfg)
}

func TestQoderCheckinSlotInNextWake(t *testing.T) {
	now := time.Now()
	h := (now.Hour() + 2) % 24
	s := qoderOnlyScheduler(t, Config{QoderCheckinHours: []int{h}, QoderKeepaliveDisabled: true})
	at, kinds := s.nextWake(now)
	if at.IsZero() || len(kinds) != 1 || kinds[0] != taskQoderCheckin {
		t.Fatalf("at=%v kinds=%v", at, kinds)
	}
	if at.Hour() != h {
		t.Fatalf("hour=%d want %d", at.Hour(), h)
	}
}

func TestQoderKeepaliveSlotInNextWake(t *testing.T) {
	now := time.Now()
	h := (now.Hour() + 3) % 24
	s := qoderOnlyScheduler(t, Config{
		QoderCheckinDisabled: true,
		QoderKeepaliveHours:  []int{h},
	})
	at, kinds := s.nextWake(now)
	if at.IsZero() || len(kinds) != 1 || kinds[0] != taskQoderKeepalive {
		t.Fatalf("at=%v kinds=%v", at, kinds)
	}
}

func TestQoderDisabledNoSlots(t *testing.T) {
	s := qoderOnlyScheduler(t, Config{
		QoderCheckinDisabled:   true,
		QoderKeepaliveDisabled: true,
	})
	if at, kinds := s.nextWake(time.Now()); !at.IsZero() || len(kinds) != 0 {
		t.Fatalf("全部禁用仍排程：at=%v kinds=%v", at, kinds)
	}
}

func TestQoderOpsNilNoSlots(t *testing.T) {
	s := New(Config{
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	})
	if at, kinds := s.nextWake(time.Now()); !at.IsZero() || len(kinds) != 0 {
		t.Fatalf("QoderOps=nil 仍排程：at=%v kinds=%v", at, kinds)
	}
}
