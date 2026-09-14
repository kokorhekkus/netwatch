package probe

import (
	"context"
	"sync"
	"time"
)

// IdleThresholdBps is the rate below which the link counts as unused.
//
// Running a capacity test while somebody is on a call both ruins their call
// and produces a meaningless number, since the test would be competing with
// real traffic for the same bottleneck.
const IdleThresholdBps = 150_000

// IdleFor is how long the link must stay quiet before a heavy probe may run.
const IdleFor = 60 * time.Second

// IdleMonitor tracks whether the link is carrying other traffic.
type IdleMonitor struct {
	iface string

	mu       sync.RWMutex
	rateBps  int64
	idleFrom time.Time
	lastSeen ifBytes
	lastAt   time.Time
}

func NewIdleMonitor(iface string) *IdleMonitor {
	return &IdleMonitor{iface: iface}
}

// Run samples interface counters until ctx is cancelled.
func (m *IdleMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		cur, err := interfaceBytes(m.iface)
		if err != nil {
			continue
		}
		now := time.Now()

		m.mu.Lock()
		if !m.lastAt.IsZero() {
			secs := now.Sub(m.lastAt).Seconds()
			delta := (cur.rx - m.lastSeen.rx) + (cur.tx - m.lastSeen.tx)
			if secs > 0 && delta >= 0 {
				m.rateBps = int64(float64(delta*8) / secs)
				if m.rateBps < IdleThresholdBps {
					if m.idleFrom.IsZero() {
						m.idleFrom = now
					}
				} else {
					m.idleFrom = time.Time{}
				}
			}
		}
		m.lastSeen, m.lastAt = cur, now
		m.mu.Unlock()
	}
}

// Idle reports whether the link has been quiet long enough for a heavy probe.
func (m *IdleMonitor) Idle() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.idleFrom.IsZero() && time.Since(m.idleFrom) >= IdleFor
}

// RateBps is the most recent observed throughput, for display.
func (m *IdleMonitor) RateBps() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rateBps
}

// Budget caps how much data heavy probes may spend per calendar month.
//
// Without a hard ceiling an automated capacity test is a slow leak against a
// data cap, and on a metered connection a genuine bill. It is deliberately a
// stop, not a warning.
type Budget struct {
	mu    sync.Mutex
	limit int64
	month string
	used  int64
}

func NewBudget(limitBytes int64) *Budget {
	return &Budget{limit: limitBytes, month: currentMonth()}
}

func currentMonth() string { return time.Now().Format("2006-01") }

// Restore seeds usage already recorded for the current month.
func (b *Budget) Restore(month string, used int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if month == currentMonth() {
		b.month, b.used = month, used
	}
}

// Allows reports whether a probe expected to cost `cost` may proceed.
func (b *Budget) Allows(cost int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	return b.used+cost <= b.limit
}

// Charge records actual spend.
func (b *Budget) Charge(n int64) (month string, used int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	b.used += n
	return b.month, b.used
}

func (b *Budget) State() (month string, used, limit int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	return b.month, b.used, b.limit
}

func (b *Budget) rollover() {
	if m := currentMonth(); m != b.month {
		b.month, b.used = m, 0
	}
}
