package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// The local model device is EXCLUSIVE, like a CPU core (operator 2026-09-25):
// the accelerator and the resident weights execute one session's work at a
// time. The device queue is the run queue in front of it — priority classes,
// FIFO within a class, a depth limit that refuses explicitly instead of
// waiting forever. Time slicing (Q3) and fair share (Q7) extend this type;
// they do not replace it.

// Class is a request's priority class. Lower value = served first.
type Class int

const (
	ClassInterrupt Class = iota
	ClassSteering
	ClassInteractive
	ClassBatch
	numClasses
)

var classNames = [...]string{"interrupt", "steering", "interactive", "batch"}

func (c Class) String() string {
	if c < 0 || c >= numClasses {
		return "unknown"
	}
	return classNames[c]
}

// ParseClass reads a class name; empty means def.
func ParseClass(s string, def Class) (Class, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return def, nil
	}
	for i, n := range classNames {
		if s == n {
			return Class(i), nil
		}
	}
	return def, fmt.Errorf("unknown class %q (want interrupt, steering, interactive or batch)", s)
}

// ErrQueueFull is the explicit refusal when the run queue is at its limit.
var ErrQueueFull = errors.New("model queue is full")

// DefaultQueueLimit bounds the waiting requests across all classes.
const DefaultQueueLimit = 64

type waiter struct {
	class     Class
	principal string
	ready     chan struct{}
	granted   bool
}

// Device is the exclusive run queue for one local model engine.
type Device struct {
	mu      sync.Mutex
	busy    bool
	waiting [numClasses][]*waiter
	limit   int

	// last is the principal whose work the device served most recently;
	// OnSwitch runs (while the device is held) before a different
	// principal's work starts, so caches never cross principals (Q8).
	last     string
	OnSwitch func(ctx context.Context, from, to string) error

	// Counters for the live view.
	served   uint64
	switches uint64
}

// NewDevice returns a device queue with the given depth limit (<= 0 uses
// DefaultQueueLimit).
func NewDevice(limit int) *Device {
	if limit <= 0 {
		limit = DefaultQueueLimit
	}
	return &Device{limit: limit}
}

// Acquire waits for the device. It returns the release func and how long
// the caller waited, ErrQueueFull when the queue is at its limit, or the
// context error when the caller gave up while waiting.
func (d *Device) Acquire(ctx context.Context, class Class, principal string) (func(), time.Duration, error) {
	if class < 0 || class >= numClasses {
		class = ClassInteractive
	}
	start := time.Now()
	d.mu.Lock()
	if !d.busy && d.queuedLocked() == 0 {
		d.busy = true
		d.mu.Unlock()
		if err := d.switchTo(ctx, principal); err != nil {
			d.release()
			return nil, time.Since(start), err
		}
		return d.releaseFunc(), 0, nil
	}
	if d.queuedLocked() >= d.limit {
		d.mu.Unlock()
		return nil, 0, ErrQueueFull
	}
	w := &waiter{class: class, principal: principal, ready: make(chan struct{})}
	d.waiting[class] = append(d.waiting[class], w)
	d.mu.Unlock()

	select {
	case <-w.ready:
		if err := d.switchTo(ctx, principal); err != nil {
			d.release()
			return nil, time.Since(start), err
		}
		return d.releaseFunc(), time.Since(start), nil
	case <-ctx.Done():
		d.mu.Lock()
		if w.granted {
			// Granted in the race window: hand the device on.
			d.mu.Unlock()
			d.release()
			return nil, time.Since(start), ctx.Err()
		}
		d.removeLocked(w)
		d.mu.Unlock()
		return nil, time.Since(start), ctx.Err()
	}
}

func (d *Device) switchTo(ctx context.Context, principal string) error {
	d.mu.Lock()
	from, hook := d.last, d.OnSwitch
	d.mu.Unlock()
	// Do not publish the new owner until eviction succeeds. A failed attempt
	// must retry eviction on the next acquisition, including after startup.
	if from != principal && hook != nil {
		if err := hook(ctx, from, principal); err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last = principal
	d.served++
	if from != "" && from != principal {
		d.switches++
	}
	return nil
}

func (d *Device) releaseFunc() func() {
	var once sync.Once
	return func() { once.Do(d.release) }
}

func (d *Device) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for c := Class(0); c < numClasses; c++ {
		if q := d.waiting[c]; len(q) > 0 {
			w := q[0]
			d.waiting[c] = q[1:]
			w.granted = true
			close(w.ready)
			return
		}
	}
	d.busy = false
}

func (d *Device) queuedLocked() int {
	n := 0
	for _, q := range d.waiting {
		n += len(q)
	}
	return n
}

func (d *Device) removeLocked(w *waiter) {
	q := d.waiting[w.class]
	for i, x := range q {
		if x == w {
			d.waiting[w.class] = append(q[:i:i], q[i+1:]...)
			return
		}
	}
}

// DeviceStats is the live view of the run queue.
type DeviceStats struct {
	Busy     bool           `json:"busy"`
	Queued   map[string]int `json:"queued"`
	Limit    int            `json:"limit"`
	Served   uint64         `json:"served"`
	Switches uint64         `json:"principal_switches"`
}

// Stats snapshots the queue.
func (d *Device) Stats() DeviceStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := map[string]int{}
	for c, list := range d.waiting {
		q[Class(c).String()] = len(list)
	}
	return DeviceStats{Busy: d.busy, Queued: q, Limit: d.limit, Served: d.served, Switches: d.switches}
}
