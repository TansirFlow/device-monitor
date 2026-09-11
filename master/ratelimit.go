package main

import (
	"sync"
	"time"
)

// rateLimiter 是极简令牌桶，按 key（IP 或 节点名）限流。
// 目的是让一台被入侵/写错的节点无法撑爆 master 的磁盘。
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64
	burst   float64
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec float64) *rateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = 5
	}
	burst := ratePerSec * 4
	if burst < 10 {
		burst = 10
	}
	return &rateLimiter{
		buckets: map[string]*tokenBucket{},
		rate:    ratePerSec,
		burst:   burst,
	}
}

func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) > 4096 {
		l.sweepLocked(now)
	}

	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &tokenBucket{tokens: l.burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *rateLimiter) sweepLocked(now time.Time) {
	cut := now.Add(-10 * time.Minute)
	for k, b := range l.buckets {
		if b.last.Before(cut) {
			delete(l.buckets, k)
		}
	}
}
