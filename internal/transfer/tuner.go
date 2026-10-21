package transfer

import (
	"context"
	"sync"
	"time"
)

// The sender opens all its data connections up front (idle TCP connections
// cost nothing) but lets only the first `limit` of them pull work. The tuner
// raises the limit while throughput keeps improving. This avoids any protocol
// for adding streams mid-transfer: every stream still ends with an END frame.

const (
	// AutoStreamsMax is how many connections auto mode opens; tuning starts at autoStreamsStart.
	AutoStreamsMax   = 16
	autoStreamsStart = 4
	// tuneEvery is the measuring window; Wi-Fi throughput is noisy, so not too short.
	tuneEvery = 1500 * time.Millisecond
	// tuneGain is the improvement that justifies more streams.
	tuneGain = 1.08
)

// gate holds back streams beyond the current limit.
type gate struct {
	mu    sync.Mutex
	limit int
	open  bool          // queue drained: let everyone through to send END
	ch    chan struct{} // closed and replaced whenever the limit changes
}

func newGate(limit int) *gate { return &gate{limit: limit, ch: make(chan struct{})} }

// wait blocks until stream i may pull work. It returns false if ctx ends first.
func (g *gate) wait(ctx context.Context, i int) bool {
	for {
		g.mu.Lock()
		if i < g.limit || g.open {
			g.mu.Unlock()
			return true
		}
		ch := g.ch
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

func (g *gate) setLimit(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n != g.limit {
		g.limit = n
		close(g.ch)
		g.ch = make(chan struct{})
	}
}

func (g *gate) current() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// release lets every stream through; called once the work queue is empty.
func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.open = true
		close(g.ch)
		g.ch = make(chan struct{})
	}
}

// tuner doubles the active stream count while each doubling still buys at
// least tuneGain more throughput, then settles on the best count it saw.
type tuner struct {
	g        *gate
	max      int
	best     float64
	bestN    int
	frozen   bool
	prevDone int64
	prevAt   time.Time
}

func newTuner(g *gate, max int) *tuner { return &tuner{g: g, max: max} }

// observe takes a progress sample and returns the goodput (bytes/s) over the
// last window, or 0 if the window isn't complete.
func (t *tuner) observe(done int64, now time.Time) float64 {
	if t.prevAt.IsZero() {
		t.prevDone, t.prevAt = done, now
		return 0
	}
	dt := now.Sub(t.prevAt).Seconds()
	if dt <= 0 {
		return 0
	}
	rate := float64(done-t.prevDone) / dt
	t.prevDone, t.prevAt = done, now
	if rate > 0 {
		t.step(rate)
	}
	return rate
}

func (t *tuner) step(rate float64) {
	if t.frozen {
		return
	}
	n := t.g.current()
	if t.bestN == 0 || rate > t.best*tuneGain {
		t.best, t.bestN = rate, n
		if n >= t.max {
			t.frozen = true
			return
		}
		t.g.setLimit(min(n*2, t.max))
		return
	}
	// More streams didn't help: go back to the best count and stay there.
	t.g.setLimit(t.bestN)
	t.frozen = true
}
