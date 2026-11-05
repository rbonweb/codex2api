package auth

import (
	"time"
)

// 调度模式（remaining_quota / fill_first / round_robin 的 healthy 桶）的排序键是
// "账号已用配额百分比"，来源是 Codex 的 5h/7d 用量窗口（wham 探针、响应头）。

// usagePercentForScheduling 返回调度排序用的已用配额百分比。
func (a *Account) usagePercentForScheduling() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usagePercentForSchedulingLocked(time.Now())
}

func (a *Account) usagePercentForSchedulingLocked(now time.Time) float64 {
	if a.UsagePercent7dValid {
		return a.UsagePercent7d
	}
	return 0
}
