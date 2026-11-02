package instabox

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Spec is the size of a machine.
type Spec struct {
	VCPUs   int `json:"vcpus"`
	MemMiB  int `json:"memMiB"`
	RootGiB int `json:"rootDiskGiB"`
	DiskGiB int `json:"persistentDiskGiB"` // /data
}

// Usage is one machine's life, as far as billing goes: from the create
// request to the delete that returned.
type Usage struct {
	Machine   string    `json:"machine"`
	Name      string    `json:"name"`
	Spec      Spec      `json:"spec"`
	From      string    `json:"from"` // the snapshot it started from
	Requested time.Time `json:"requested"`
	Ready     time.Time `json:"ready"`
	Deleted   time.Time `json:"deleted"`
	// Saved is the snapshot made from it, if any.
	Saved string `json:"saved,omitempty"`
	// Spare marks the making of a machine that was then paused to wait (the
	// running part of its life: creating it and waiting for its agent).
	Spare bool `json:"spare,omitempty"`
	// Resumed marks a machine that came from the paused spares; PausedSec is
	// how long it had been paused, which costs storage only.
	Resumed   bool    `json:"resumed,omitempty"`
	PausedSec float64 `json:"paused_sec,omitempty"`
}

// Seconds is how long the machine existed; for one not deleted yet, up to now.
func (u Usage) Seconds() float64 {
	end := u.Deleted
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(u.Requested).Seconds()
}

// Prices are InstaBox's unit prices: per vCPU, per GiB of memory and per GiB
// of storage per second, and per GiB of data leaving the machine.
type Prices struct {
	VCPUSec       float64 `json:"vcpu_sec"`
	MemGiBSec     float64 `json:"mem_gib_sec"`
	StorageGiBSec float64 `json:"storage_gib_sec"`
	EgressGiB     float64 `json:"egress_gib"`
}

// InstaBoxPrices are the unit prices on InstaBox's price page, read on
// 2026-10-29. They change; the benchmark takes its own from a file.
var InstaBoxPrices = Prices{VCPUSec: 0.000001111, MemGiBSec: 0.000000625, StorageGiBSec: 0.000000031, EgressGiB: 0.02}

// PerSecond is what a machine of this size costs per second.
func (p Prices) PerSecond(s Spec) float64 {
	return float64(s.VCPUs)*p.VCPUSec + float64(s.MemMiB)/1024*p.MemGiBSec + float64(s.RootGiB+s.DiskGiB)*p.StorageGiBSec
}

// Cost is what the machine's life cost, without egress: its running time at
// the full rate, and a paused spare's waiting time at the storage rate alone.
func (p Prices) Cost(u Usage) float64 {
	return u.Seconds()*p.PerSecond(u.Spec) + u.PausedSec*float64(u.Spec.RootGiB+u.Spec.DiskGiB)*p.StorageGiBSec
}

// Summary adds up usages.
type Summary struct {
	Machines int
	Seconds  float64
	Cost     float64
}

// Summarize adds up the machine time and cost of usages.
func (p Prices) Summarize(us []Usage) Summary {
	var s Summary
	for _, u := range us {
		s.Machines++
		s.Seconds += u.Seconds()
		s.Cost += p.Cost(u)
	}
	return s
}

// WriteUsage writes one JSON object per line.
func WriteUsage(w io.Writer, us []Usage) error {
	enc := json.NewEncoder(w)
	for _, u := range us {
		if err := enc.Encode(u); err != nil {
			return err
		}
	}
	return nil
}

// ReadUsage reads a file WriteUsage made.
func ReadUsage(path string) ([]Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Usage
	dec := json.NewDecoder(f)
	for dec.More() {
		var u Usage
		if err := dec.Decode(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// usageLog is what the provider keeps.
type usageLog struct {
	mu sync.Mutex
	us []*Usage
}

func (l *usageLog) add(u *Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.us = append(l.us, u)
}

// snapshot copies the entries, taking the lock that guards their fields.
func (l *usageLog) snapshot() []Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Usage, len(l.us))
	for i, u := range l.us {
		out[i] = *u
	}
	return out
}

func (l *usageLog) set(u *Usage, f func(*Usage)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(u)
}
