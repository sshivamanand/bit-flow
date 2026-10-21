// Package units parses and formats byte sizes, rates and durations.
package units

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var multipliers = map[string]float64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "kib": 1 << 10,
	"m": 1e6, "mb": 1e6, "mib": 1 << 20,
	"g": 1e9, "gb": 1e9, "gib": 1 << 30,
	"t": 1e12, "tb": 1e12, "tib": 1 << 40,
}

// ParseSize parses sizes like "4MiB", "10GB", "1.5g" or "4096".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult, ok := multipliers[strings.ToLower(strings.TrimSpace(s[i:]))]
	if !ok {
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	return int64(num * mult), nil
}

// Bytes formats n using decimal units (GB, not GiB), matching how drives and links are sold.
func Bytes(n int64) string {
	f := float64(n)
	switch {
	case f >= 1e12:
		return fmt.Sprintf("%.2f TB", f/1e12)
	case f >= 1e9:
		return fmt.Sprintf("%.2f GB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1f MB", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1f KB", f/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

// Rate formats a bytes-per-second value.
func Rate(bps float64) string {
	return Bytes(int64(bps)) + "/s"
}

// Bits formats a bytes-per-second value as a line rate in Gbit/s or Mbit/s.
func Bits(bps float64) string {
	b := bps * 8
	if b >= 1e9 {
		return fmt.Sprintf("%.2f Gbit/s", b/1e9)
	}
	return fmt.Sprintf("%.0f Mbit/s", b/1e6)
}

// Duration formats d compactly: 1h02m, 3m04s, 12.3s.
func Duration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
