package whatsmeow

import (
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Per-chat limit on answering retry receipts with a resend. WhatsApp's
// anti-abuse looks at the aggregate rate of resends into a chat, not any one
// device's: when a group migrates to LID addressing, hundreds of devices can
// retry the same messages at once, per-device and per-message caps never
// engage, and the account gets locked. Ported from whatsapp-rust's
// ResendRateLimiter.
const (
	// resendBurst is how many resends one chat may get before the refill
	// rate gates it; a chat's first activity is never throttled.
	resendBurst = 20
	// resendRefillPerMinute is the sustained ceiling per chat.
	resendRefillPerMinute = 10
	// resendLimiterMaxChats bounds the bucket map. Dropping buckets only
	// forgives rate (they come back full), never over-restricts.
	resendLimiterMaxChats = 4096
)

type resendBucket struct {
	tokens     float64
	lastRefill time.Time
}

// take refills for the time since the last access, then tries to take one
// token.
func (b *resendBucket) take(now time.Time, burst, refillPerSec float64) bool {
	if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		b.tokens = min(b.tokens+elapsed*refillPerSec, burst)
	}
	b.lastRefill = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type resendLimiter struct {
	lock    sync.Mutex
	buckets map[types.JID]*resendBucket
}

// allow consumes one resend token for chat, reporting whether the resend may
// go out.
func (l *resendLimiter) allow(chat types.JID, now time.Time) bool {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.buckets == nil || len(l.buckets) >= resendLimiterMaxChats {
		l.buckets = make(map[types.JID]*resendBucket)
	}
	bucket, ok := l.buckets[chat]
	if !ok {
		bucket = &resendBucket{tokens: resendBurst, lastRefill: now}
		l.buckets[chat] = bucket
	}
	return bucket.take(now, resendBurst, resendRefillPerMinute/60.0)
}
