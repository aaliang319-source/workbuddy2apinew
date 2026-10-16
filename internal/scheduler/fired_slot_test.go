package scheduler

import (
	"testing"
	"time"
)

// TestNextFireKindSkipsFiredSlot 回归：同一小时窗口不得重复执行。
// 执行后（MarkFired）重排，同槽必须作废 → 下次落点是次日同一小时，
// 而不是同日内重掷出的另一个落点（线上曾出现 checkin 09:15/09:38/09:53 三连跑）。
func TestNextFireKindSkipsFiredSlot(t *testing.T) {
	r := NewRandomizer(55)
	now := time.Date(2026, 10, 14, 9, 15, 0, 0, time.Local)

	first := r.nextFireKind("checkin", now, []int{9})
	if first.Day() != 14 || first.Hour() != 9 {
		t.Fatalf("首次落点应为今日 9 点窗口，got %v", first)
	}

	// 模拟 09:15 执行完成后重排。
	r.MarkFired("checkin", first)
	next := r.nextFireKind("checkin", first, []int{9})
	if !sameLocalDay(next, first.AddDate(0, 0, 1)) || next.Hour() != 9 {
		t.Fatalf("执行后同槽应作废、落次日 9 点，got %v", next)
	}

	// 无 kind 的旧路径不受影响（仍允许同槽取值，供一次性计算/测试）。
	plain := r.nextFire(first, []int{9})
	if plain.Before(first) {
		t.Fatalf("无 kind 路径不应回退到过去，got %v", plain)
	}

	// 其它任务类型不受 checkin 的槽位记录影响。
	other := r.nextFireKind("activity", first, []int{9})
	if other.Day() != 14 || other.Hour() != 9 {
		t.Fatalf("其它 kind 不应被连坐，got %v", other)
	}
}
