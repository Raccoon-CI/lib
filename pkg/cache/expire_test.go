package cache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEntriesExpireAfterTheyHaveNotBeenUsedForAWhile(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now()
	clock := t0
	e.svc.now = func() time.Time { return clock }
	e.save("old", "v", strings.Repeat("o", 30), 16)
	clock = t0.Add(5 * 24 * time.Hour)
	e.save("kept", "v", strings.Repeat("k", 20), 16)
	clock = t0.Add(6 * 24 * time.Hour)
	e.save("stale", "v", strings.Repeat("s", 10), 16)
	clock = t0.Add(8 * 24 * time.Hour)
	if !e.has("v", "old") { // a hit renews it
		t.Fatal("old is missing")
	}

	// Day 14, a week back is day 7: "kept" (day 5) and "stale" (day 6) have not
	// been used since; "old" was hit on day 8.
	clock = t0.Add(14 * 24 * time.Hour)
	n, freed, err := e.svc.Expire(context.Background(), clock.Add(-7*24*time.Hour))
	if err != nil || n != 2 || freed != 30 {
		t.Fatalf("Expire = %d entries, %d bytes, %v; want kept (20) and stale (10) gone", n, freed, err)
	}
	if !e.has("v", "old") || e.has("v", "kept") || e.has("v", "stale") {
		t.Fatal("the wrong entries went")
	}
	if got := e.svc.Entries(); len(got) != 1 || got[0].Key != "old" || got[0].Size != 30 {
		t.Fatalf("entries = %+v", got)
	}
	// Saved with the index: a new process sees the same.
	e2 := start(t, filepath.Dir(e.index), false)
	if got := e2.svc.Entries(); len(got) != 1 || got[0].Key != "old" {
		t.Fatalf("after restart: %+v", got)
	}
}

func TestEvictRemovesOneEntryAndItsObject(t *testing.T) {
	e := newEnv(t)
	id := e.save("x", "v", "data", 16)
	if err := e.svc.Evict(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if e.has("v", "x") || len(e.svc.Entries()) != 0 {
		t.Fatal("the entry is still there")
	}
	if err := e.svc.Evict(context.Background(), id); err != nil {
		t.Fatalf("evicting twice: %v", err)
	}
}

func TestLastUseReachesTheDiskSoARestartDoesNotMakeABusyEntryLookStale(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now()
	clock := t0
	e.svc.now = func() time.Time { return clock }
	e.save("busy", "v", "data", 16)

	clock = t0.Add(6 * 24 * time.Hour)
	if !e.has("v", "busy") { // a hit; the first one after a long time is saved at once
		t.Fatal("busy is missing")
	}
	onDisk, err := ReadIndex(e.index)
	if err != nil || len(onDisk) != 1 || onDisk[0].Used.Before(t0.Add(5*24*time.Hour)) {
		t.Fatalf("on disk: %+v, %v: the hit was not saved", onDisk, err)
	}

	// A hit a minute later is not worth a write... but Close saves it.
	clock = clock.Add(time.Minute)
	e.has("v", "busy")
	if onDisk, _ = ReadIndex(e.index); !onDisk[0].Used.Before(clock) {
		t.Fatal("a hit a minute after the last save should not have rewritten the index")
	}
	e.svc.Close()
	if onDisk, _ = ReadIndex(e.index); onDisk[0].Used.Before(clock) {
		t.Fatalf("Close must save the last use: %+v", onDisk)
	}
}

func TestReadIndexOfAMissingFileIsEmpty(t *testing.T) {
	got, err := ReadIndex(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestIdleIsFalseWhileAnUploadIsReserved(t *testing.T) {
	e := newEnv(t)
	if !e.svc.Idle() {
		t.Fatal("a new service is idle")
	}
	if code, _ := e.reserve("k", "v"); code != 201 {
		t.Fatalf("reserve = %d", code)
	}
	if e.svc.Idle() {
		t.Fatal("an upload is in progress")
	}
}
