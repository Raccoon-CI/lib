package cache

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tick gives the service a clock that moves a second per reading, so "least
// recently used" is well defined. It starts now, not at a fixed date: the
// upload links the service signs expire against the real clock, so a clock set
// in the past made every test using tick fail once the date had gone by.
func tick(e *env) {
	t0 := time.Now()
	n := 0
	e.svc.now = func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }
}

func (e *env) has(version, key string) bool {
	h, _ := e.lookup(version, key)
	return h != nil
}

func TestCapEvictsTheLeastRecentlyUsed(t *testing.T) {
	e := newEnv(t)
	tick(e)
	e.svc.maxBytes = 100
	fill := func(c byte) string { return strings.Repeat(string(c), 40) }

	e.save("a", "v", fill('a'), 16)
	e.save("b", "v", fill('b'), 16)
	if !e.has("v", "a") { // a is now newer than b
		t.Fatal("a is missing")
	}
	e.save("c", "v", fill('c'), 16) // 120 bytes: one must go

	if e.has("v", "b") {
		t.Error("b was used least recently and should have been evicted")
	}
	if !e.has("v", "a") || !e.has("v", "c") {
		t.Error("a or c was evicted")
	}
	// The index and the store agree.
	e2 := start(t, filepath.Dir(e.index), false)
	if e2.has("v", "b") || !e2.has("v", "a") {
		t.Error("the eviction was not saved with the index")
	}
}

func TestEntryLargerThanTheCapIsRefused(t *testing.T) {
	e := newEnv(t)
	e.svc.maxBytes = 50

	// Told up front by the size the client announces...
	b, _ := json.Marshal(map[string]any{"key": "big", "version": "v", "cacheSize": 60})
	resp, body := e.do("POST", "caches", e.tok, map[string]string{"Content-Type": "application/json"}, strings.NewReader(string(b)))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "limit") {
		t.Errorf("reserve = %d %s", resp.StatusCode, body)
	}
	// ...and at commit, when the announced size was wrong.
	_, id := e.reserve("big2", "v")
	e.patch(id, 0, strings.Repeat("x", 60))
	if c, body := e.commit(id, 60); c != 400 || !strings.Contains(body, "limit") {
		t.Errorf("commit = %d %s", c, body)
	}
	if e.has("v", "big2") {
		t.Error("the entry over the cap was stored")
	}
}

// Two entries committed at the same moment each find room for themselves; the
// cap must still hold once both are in.
func TestCapHoldsForConcurrentCommits(t *testing.T) {
	e := newEnv(t)
	tick(e)
	e.svc.maxBytes = 100
	data := strings.Repeat("x", 60)

	_, idB := e.reserve("b", "v")
	e.patch(idB, 0, data)
	inHook := false
	e.svc.free = func() (int64, error) {
		if !inHook { // a's room check: b commits while a is still deciding
			inHook = true
			if c, body := e.commit(idB, len(data)); c != 204 {
				t.Errorf("b: %d %s", c, body)
			}
		}
		return 1 << 40, nil
	}
	_, idA := e.reserve("a", "v")
	e.patch(idA, 0, data)
	if c, body := e.commit(idA, len(data)); c != 204 {
		t.Fatalf("a: %d %s", c, body)
	}

	n := 0
	for _, k := range []string{"a", "b"} {
		if e.has("v", k) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d entries of 60 bytes under a cap of 100", n)
	}
}

// With v2 the data is stored before Finalize, so the cap is enforced after it.
func TestV2CapEvictsAndRefuses(t *testing.T) {
	e := start(t, t.TempDir(), true)
	tick(e)
	e.svc.maxBytes = 100
	save := func(key string, n int) (bool, string) {
		ok, u, msg := e.create(key, "v")
		if !ok {
			t.Fatalf("create %s: %s", key, msg)
		}
		if c := put(t, u, "", blobHeaders, strings.Repeat("x", n)); c != 201 {
			t.Fatalf("Put Blob = %d", c)
		}
		ok, _, msg = e.finalizeV2(key, "v", n)
		return ok, msg
	}
	for _, k := range []string{"a", "b"} {
		if ok, msg := save(k, 40); !ok {
			t.Fatalf("%s: %s", k, msg)
		}
	}
	if ok, msg := save("c", 40); !ok { // 120 bytes: a, the oldest, goes
		t.Fatalf("c: %s", msg)
	}
	if m, _ := e.download("a", "v"); m != "" {
		t.Error("a should have been evicted")
	}
	if m, _ := e.download("c", "v"); m != "c" {
		t.Error("c is missing")
	}
	if ok, msg := save("big", 150); ok || !strings.Contains(msg, "limit") {
		t.Errorf("an entry over the cap: ok=%v %q", ok, msg)
	}
	if m, _ := e.download("b", "v"); m != "b" {
		t.Error("refusing the big entry must not evict the others")
	}
}

func TestLowDiskEvictsThenRefuses(t *testing.T) {
	e := newEnv(t)
	tick(e)
	disk := int64(freeMargin + 100) // what the disk would have free when empty
	e.svc.free = func() (int64, error) {
		e.svc.mu.Lock()
		defer e.svc.mu.Unlock()
		var used int64
		for _, r := range e.svc.done {
			used += r.Size
		}
		return disk - used, nil
	}

	e.save("a", "v", strings.Repeat("a", 60), 16)
	e.save("b", "v", strings.Repeat("b", 60), 16) // does not fit beside a
	if e.has("v", "a") || !e.has("v", "b") {
		t.Error("a should have been evicted to make room for b")
	}

	// Bigger than the disk could ever hold: nothing left to evict but b, and
	// that is not enough either.
	_, id := e.reserve("huge", "v")
	e.patch(id, 0, strings.Repeat("h", 300))
	c, body := e.commit(id, 300)
	if c != 507 || !strings.Contains(body, "not enough room") {
		t.Errorf("commit = %d %s", c, body)
	}
}
