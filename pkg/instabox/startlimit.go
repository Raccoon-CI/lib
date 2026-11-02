package instabox

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// InstaBox lets an organization start a limited number of machines a minute
// (10 on the Pro plan) and refuses the rest with 429. StartLimit makes a
// caller that starts many machines (the server, with several runs at once)
// wait its turn instead of failing: it counts its own starts in a sliding
// window, and after a 429 it holds every start for a while, because someone
// else of the organization may be using the limit too.

// StartLimit spaces machine starts. The zero value, and a nil *StartLimit,
// allow everything.
type StartLimit struct {
	PerMinute int           // 0: no limit of our own
	Window    time.Duration // default 62 s, a little over the minute the limit speaks of
	Hold      time.Duration // how long starts wait after a 429; default 20 s

	mu        sync.Mutex
	starts    []time.Time
	holdUntil time.Time
}

func (l *StartLimit) window() time.Duration {
	if l.Window > 0 {
		return l.Window
	}
	return 62 * time.Second
}

func (l *StartLimit) hold() time.Duration {
	if l.Hold > 0 {
		return l.Hold
	}
	return 20 * time.Second
}

// reserve takes a start if one is free; otherwise it says how long to wait.
func (l *StartLimit) reserve(now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.holdUntil) {
		return false, l.holdUntil.Sub(now)
	}
	if l.PerMinute <= 0 {
		return true, 0
	}
	i := 0
	for i < len(l.starts) && now.Sub(l.starts[i]) > l.window() {
		i++
	}
	l.starts = l.starts[i:]
	if len(l.starts) >= l.PerMinute {
		// the start that has to leave the window before one is free
		return false, max(l.starts[len(l.starts)-l.PerMinute].Add(l.window()).Sub(now), 10*time.Millisecond)
	}
	l.starts = append(l.starts, now)
	return true, 0
}

// Wait blocks until a start may be made and counts it. It returns early with
// the context's error. `waited` is how long it blocked.
func (l *StartLimit) Wait(ctx context.Context) (waited time.Duration, err error) {
	if l == nil {
		return 0, nil
	}
	began := time.Now()
	for {
		ok, wait := l.reserve(time.Now())
		if ok {
			return time.Since(began), nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return time.Since(began), ctx.Err()
		case <-t.C:
		}
	}
}

// Refused tells the limit that InstaBox answered 429: no start is made for
// the hold time.
func (l *StartLimit) Refused() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holdUntil = time.Now().Add(l.hold())
}

// IsRateLimit reports whether err is InstaBox refusing for too many starts.
func IsRateLimit(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusTooManyRequests
}
