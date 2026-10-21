package transfer

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Compression only pays when the link, not the CPU, is the bottleneck.
// Measured on an Apple M4: zstd "fastest" packs executables/assets at ~480 MB/s
// per core to ~45%, and rejects incompressible data at ~9 GB/s. So over Wi-Fi
// (≤ 150 MB/s) it's nearly free bandwidth; on a multi-GB/s cable it would
// become the bottleneck, and auto mode turns it off.

// Compression modes.
const (
	CompressAuto = "auto" // on until it stops paying off
	CompressOn   = "on"
	CompressOff  = "off"
)

const (
	// A compressed block must be at most this fraction of the raw size to be sent compressed.
	compressKeepRatio = 0.9
	// A file whose first blocks don't shrink is sent raw from then on.
	compressFileStrikes = 2
	// Auto mode judges compression over windows of at least this many bytes.
	compressWindow = 64 << 20
	// Below this overall saving, auto mode pauses compression for compressRetry.
	compressMinSaving = 0.05
	compressRetry     = 20 * time.Second
)

// Already-compressed formats never shrink; don't spend CPU finding that out.
var incompressibleExt = map[string]bool{
	".zip": true, ".7z": true, ".rar": true, ".gz": true, ".tgz": true, ".xz": true, ".bz2": true, ".zst": true,
	".lz4": true, ".br": true, ".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".heic": true, ".avif": true,
	".gif": true, ".mp4": true, ".mkv": true, ".mov": true, ".webm": true, ".avi": true, ".mp3": true, ".aac": true,
	".ogg": true, ".opus": true, ".flac": true, ".m4a": true, ".dmg": true, ".pkg": true, ".apk": true, ".ipa": true,
	".bik": true, ".bk2": true, ".usm": true, ".xvc": true,
}

// compressor decides, block by block, whether to send zstd or raw bytes.
type compressor struct {
	mode    string
	enc     *zstd.Encoder
	enabled atomic.Bool
	strikes []atomic.Int32 // per file: blocks that didn't shrink

	tried, saved, nanos atomic.Int64 // totals, for stats and the auto controller
	winTried, winSaved  atomic.Int64 // current judging window
	pausedUntil         atomic.Int64 // unix nanos; auto mode retry time
	cpuBound            atomic.Bool  // auto mode gave up for good
}

func newCompressor(mode string, files []string) *compressor {
	c := &compressor{mode: mode, strikes: make([]atomic.Int32, len(files))}
	if mode == CompressOff {
		return c
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithZeroFrames(true))
	if err != nil {
		return c
	}
	c.enc = enc
	c.enabled.Store(true)
	for i, name := range files {
		if incompressibleExt[strings.ToLower(filepath.Ext(name))] {
			c.strikes[i].Store(compressFileStrikes)
		}
	}
	return c
}

// wants reports whether a block of file is worth trying to compress.
func (c *compressor) wants(file int) bool {
	if c.enc == nil || !c.enabled.Load() {
		return false
	}
	return file < 0 || file >= len(c.strikes) || c.strikes[file].Load() < compressFileStrikes
}

// compress returns raw compressed into dst (reusing its capacity) if that's
// worth sending. file < 0 means a pack, which has no per-file history.
func (c *compressor) compress(raw, dst []byte, file int) ([]byte, bool) {
	if c.enc == nil || !c.enabled.Load() {
		return nil, false
	}
	if file >= 0 && file < len(c.strikes) && c.strikes[file].Load() >= compressFileStrikes {
		return nil, false
	}
	start := time.Now()
	out := c.enc.EncodeAll(raw, dst[:0])
	c.nanos.Add(int64(time.Since(start)))
	c.tried.Add(int64(len(raw)))
	c.winTried.Add(int64(len(raw)))
	if float64(len(out)) > float64(len(raw))*compressKeepRatio {
		if file >= 0 && file < len(c.strikes) {
			c.strikes[file].Add(1)
		}
		return nil, false
	}
	c.saved.Add(int64(len(raw) - len(out)))
	c.winSaved.Add(int64(len(raw) - len(out)))
	return out, true
}

// adjust is called periodically in auto mode with the current goodput and
// number of active streams. It pauses compression when it isn't saving
// anything, and stops it for good when it would be slower than the link.
func (c *compressor) adjust(goodput float64, streams int) {
	if c.mode != CompressAuto || c.enc == nil || c.cpuBound.Load() {
		return
	}
	now := time.Now().UnixNano()
	if !c.enabled.Load() {
		if now >= c.pausedUntil.Load() {
			c.winTried.Store(0)
			c.winSaved.Store(0)
			c.enabled.Store(true)
		}
		return
	}
	tried, nanos := c.tried.Load(), c.nanos.Load()
	if tried > compressWindow && nanos > 0 && goodput > 0 {
		perCore := float64(tried) / (float64(nanos) / 1e9)
		capacity := perCore * float64(min(streams, runtime.NumCPU()))
		if goodput > 0.6*capacity {
			c.cpuBound.Store(true)
			c.enabled.Store(false)
			return
		}
	}
	if wt := c.winTried.Load(); wt > compressWindow {
		if float64(c.winSaved.Load())/float64(wt) < compressMinSaving {
			c.enabled.Store(false)
			c.pausedUntil.Store(now + int64(compressRetry))
		}
		c.winTried.Store(0)
		c.winSaved.Store(0)
	}
}

// decompressor is shared by all streams of a receiver session.
type decompressor struct{ dec *zstd.Decoder }

func newDecompressor(maxBlock int) (*decompressor, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)),
		zstd.WithDecoderMaxMemory(uint64(maxBlock)*2))
	if err != nil {
		return nil, err
	}
	return &decompressor{dec: dec}, nil
}

func (d *decompressor) close() { d.dec.Close() }
