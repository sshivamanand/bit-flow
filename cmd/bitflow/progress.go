package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sshivamanand/bit-flow/internal/units"
)

// meter renders a single, self-overwriting progress line with a smoothed rate.
type meter struct {
	w       io.Writer
	total   int64
	start   time.Time
	lastT   time.Time
	lastB   int64
	rate    float64
	lastLen int
}

func newMeter(w io.Writer, total int64) *meter {
	now := time.Now()
	return &meter{w: w, total: total, start: now, lastT: now}
}

func (m *meter) update(done int64, extra string) {
	now := time.Now()
	if dt := now.Sub(m.lastT).Seconds(); dt > 0 {
		inst := float64(done-m.lastB) / dt
		if m.rate == 0 {
			m.rate = inst
		} else {
			m.rate = 0.3*inst + 0.7*m.rate // EMA keeps the number readable
		}
	}
	m.lastT, m.lastB = now, done

	pct := 100.0
	if m.total > 0 {
		pct = float64(done) * 100 / float64(m.total)
	}
	eta := "--"
	if m.rate > 0 && done < m.total {
		eta = units.Duration(time.Duration(float64(m.total-done) / m.rate * float64(time.Second)))
	}
	line := fmt.Sprintf("  %s / %s  %5.1f%%  %s  %s  ETA %s",
		units.Bytes(done), units.Bytes(m.total), pct, bar(pct, 24), units.Rate(m.rate), eta)
	if extra != "" {
		line += "  · " + extra
	}
	pad := ""
	if n := m.lastLen - len(line); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	m.lastLen = len(line)
	fmt.Fprint(m.w, "\r"+line+pad)
}

func (m *meter) finish() { fmt.Fprintln(m.w) }

func bar(pct float64, width int) string {
	n := int(pct / 100 * float64(width))
	n = max(0, min(width, n))
	return "[" + strings.Repeat("█", n) + strings.Repeat("·", width-n) + "]"
}

func summary(bytes int64, elapsed time.Duration) string {
	rate := float64(bytes) / elapsed.Seconds()
	return fmt.Sprintf("%s in %s  —  %s (%s)", units.Bytes(bytes), units.Duration(elapsed), units.Rate(rate), units.Bits(rate))
}
