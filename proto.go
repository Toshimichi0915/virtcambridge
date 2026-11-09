// Shared-memory layout, used by the host daemon (Linux) and the guest capture (Windows).
//
// Global page:
//
//	0   u32 magic     host writes it; the guest uses it to pick our ivshmem device
//	                  among others (e.g. Looking Glass)
//	4   u32 version
//	8   u64 size
//	16  u32 channels  one channel per guest camera
//
// Channel i lives at 4096 + i*channelSize and has its own 4 KiB header:
//
//	16       u64 latest  frame<<2 | slot, published by the guest once the slot is complete
//	64+32*s  slot s: u64 seq (odd while being written), u32 width, u32 height, u32 bytes
//	256      camera name, NUL-padded (empty = unused)
//	4096+s*slotSize      slot s pixel data (I420)
package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"
)

const (
	magic       = 0x31524256 // "VBR1"
	version     = 2
	hdrSize     = 4096
	nSlots      = 3
	offChannels = 16
	offLatest   = 16
	offName     = 256
	nameLen     = 128
	maxChannels = 64
	maxDim      = 16384
)

func p64(b []byte, off int) *uint64 { return (*uint64)(unsafe.Pointer(&b[off])) }
func p32(b []byte, off int) *uint32 { return (*uint32)(unsafe.Pointer(&b[off])) }
func slotDesc(s int) int            { return 64 + 32*s }
func slotSize(ch []byte) int        { return (len(ch) - hdrSize) / nSlots &^ 4095 }
func i420Size(w, h int) int         { return w*h + 2*((w+1)/2)*((h+1)/2) }

func slotData(ch []byte, s int) []byte {
	n := slotSize(ch)
	return ch[hdrSize+s*n : hdrSize+(s+1)*n]
}

func channelCount(b []byte) int { return int(atomic.LoadUint32(p32(b, offChannels))) }

func channel(b []byte, i int) []byte {
	n := (len(b) - hdrSize) / channelCount(b) &^ 4095
	return b[hdrSize+i*n : hdrSize+(i+1)*n]
}

func ready(b []byte) bool {
	if len(b) <= hdrSize || atomic.LoadUint32(p32(b, 0)) != magic || atomic.LoadUint32(p32(b, 4)) != version {
		return false
	}
	n := channelCount(b)
	return n >= 1 && n <= maxChannels && (len(b)-hdrSize)/n >= hdrSize+nSlots*4096
}

// initHeader (host) leaves an already-initialised header alone, so restarting the
// daemon doesn't disturb a running guest.
func initHeader(b []byte, channels int) {
	if ready(b) {
		return
	}
	clear(b) // may hold an older layout
	*p64(b, 8) = uint64(len(b))
	*p32(b, offChannels) = uint32(channels)
	*p32(b, 4) = version
	atomic.StoreUint32(p32(b, 0), magic)
}

func chName(ch []byte) string {
	n := ch[offName : offName+nameLen]
	if i := bytes.IndexByte(n, 0); i >= 0 {
		n = n[:i]
	}
	return string(n)
}

func setName(ch []byte, name string) {
	n := ch[offName : offName+nameLen]
	clear(n)
	copy(n[:nameLen-1], name)
}

// writer (guest) publishes frames. Atomic stores are LOCKed instructions on x86, which
// also drain write-combining buffers, so no explicit sfence is needed.
type writer struct {
	b     []byte
	frame uint64
	slot  int
}

func newWriter(ch []byte) *writer {
	l := atomic.LoadUint64(p64(ch, offLatest))
	return &writer{b: ch, frame: l >> 2, slot: int(l&3) % nSlots}
}

// put writes into the slot after the latest one, so the host can still be copying the
// latest while we write; readFrame's seq check catches the rare overlap.
func (w *writer) put(width, height int, data []byte) bool {
	if len(data) != i420Size(width, height) || len(data) > slotSize(w.b) {
		return false
	}
	s := (w.slot + 1) % nSlots
	d := slotDesc(s)
	seq := atomic.LoadUint64(p64(w.b, d)) | 1 // odd: writing (also recovers from a crash mid-write)
	atomic.StoreUint64(p64(w.b, d), seq)
	copy(slotData(w.b, s), data)
	atomic.StoreUint32(p32(w.b, d+8), uint32(width))
	atomic.StoreUint32(p32(w.b, d+12), uint32(height))
	atomic.StoreUint32(p32(w.b, d+16), uint32(len(data)))
	atomic.StoreUint64(p64(w.b, d), seq+1) // even: done
	w.frame++
	w.slot = s
	atomic.StoreUint64(p64(w.b, offLatest), w.frame<<2|uint64(s))
	return true
}

// readFrame (host) copies the channel's latest frame into buf if it's newer than last.
// It returns the new latest marker; frame is nil if nothing new, torn, or invalid.
func readFrame(ch []byte, last uint64, buf []byte) (latest uint64, w, h int, frame []byte) {
	latest = atomic.LoadUint64(p64(ch, offLatest))
	s := int(latest & 3)
	if latest == last || s >= nSlots {
		return latest, 0, 0, nil
	}
	d := slotDesc(s)
	seq := atomic.LoadUint64(p64(ch, d))
	w = int(atomic.LoadUint32(p32(ch, d+8)))
	h = int(atomic.LoadUint32(p32(ch, d+12)))
	n := int(atomic.LoadUint32(p32(ch, d+16)))
	if seq&1 != 0 || w <= 0 || h <= 0 || w > maxDim || h > maxDim || n != i420Size(w, h) || n > slotSize(ch) {
		return latest, 0, 0, nil
	}
	frame = append(buf[:0], slotData(ch, s)[:n]...)
	if atomic.LoadUint64(p64(ch, d)) != seq {
		return latest, 0, 0, nil
	}
	return latest, w, h, frame
}

var maxSizeRe = regexp.MustCompile(`max s=(\d+)x(\d+)`)

// parseMaxSize (guest) picks the largest mode from `ffmpeg -f dshow -list_options true`.
func parseMaxSize(out string) (w, h int, ok bool) {
	for _, m := range maxSizeRe.FindAllStringSubmatch(out, -1) {
		mw, _ := strconv.Atoi(m[1])
		mh, _ := strconv.Atoi(m[2])
		if mw*mh > w*h {
			w, h = mw, mh
		}
	}
	return w, h, w > 0 && h > 0
}

// parseY4M (guest) reads the stream header ffmpeg's yuv4mpegpipe muxer emits.
func parseY4M(line string) (w, h int, err error) {
	f := strings.Fields(line)
	if len(f) == 0 || f[0] != "YUV4MPEG2" {
		return 0, 0, fmt.Errorf("not a y4m stream: %q", line)
	}
	for _, t := range f[1:] {
		switch t[0] {
		case 'W':
			w, _ = strconv.Atoi(t[1:])
		case 'H':
			h, _ = strconv.Atoi(t[1:])
		case 'C':
			if !strings.HasPrefix(t, "C420") {
				return 0, 0, fmt.Errorf("unsupported y4m colorspace %s", t)
			}
		}
	}
	if w <= 0 || h <= 0 || w > maxDim || h > maxDim {
		return 0, 0, fmt.Errorf("bad y4m size in %q", line)
	}
	return w, h, nil
}
