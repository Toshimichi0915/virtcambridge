package main

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	b := make([]byte, 128<<20)
	if ready(b) {
		t.Fatal("ready before init")
	}
	initHeader(b, 4)
	if !ready(b) || channelCount(b) != 4 {
		t.Fatal("not ready after init")
	}
	ch := channel(b, 1)

	w := newWriter(ch)
	var last uint64
	if l, _, _, f := readFrame(ch, last, nil); f != nil || l != last {
		t.Fatal("frame before any put")
	}
	for i := 0; i < 5; i++ {
		data := bytes.Repeat([]byte{byte(i)}, i420Size(1920, 1080))
		if !w.put(1920, 1080, data) {
			t.Fatal("put failed")
		}
		var fw, fh int
		var f []byte
		last, fw, fh, f = readFrame(ch, last, nil)
		if fw != 1920 || fh != 1080 || !bytes.Equal(f, data) {
			t.Fatalf("frame %d mismatch", i)
		}
		if _, _, _, f = readFrame(ch, last, nil); f != nil {
			t.Fatal("same frame read twice")
		}
	}

	// Channels don't overlap: the neighbours saw nothing.
	for _, i := range []int{0, 2, 3} {
		if _, _, _, f := readFrame(channel(b, i), 0, nil); f != nil {
			t.Fatalf("channel %d got a frame", i)
		}
	}

	// A writer that restarts continues the sequence instead of reusing markers.
	w2 := newWriter(ch)
	w2.put(2, 2, make([]byte, i420Size(2, 2)))
	if l, _, _, f := readFrame(ch, last, nil); f == nil || l <= last {
		t.Fatal("restarted writer not seen")
	}

	// Slot being written (odd seq) is rejected.
	w2.put(2, 2, make([]byte, i420Size(2, 2)))
	*p64(ch, slotDesc(w2.slot)) |= 1
	if _, _, _, f := readFrame(ch, 0, nil); f != nil {
		t.Fatal("torn frame accepted")
	}

	// Oversized frames are refused.
	if w.put(8192, 8192, make([]byte, i420Size(8192, 8192))) {
		t.Fatal("oversized frame accepted")
	}

	// Names.
	setName(ch, "OBS Virtual Camera")
	if chName(ch) != "OBS Virtual Camera" || chName(channel(b, 0)) != "" {
		t.Fatal("name mismatch")
	}

	// Re-init by a restarted host keeps the guest's state.
	l := *p64(ch, offLatest)
	initHeader(b, 8)
	if *p64(ch, offLatest) != l || channelCount(b) != 4 {
		t.Fatal("initHeader clobbered state")
	}

	// An old (v1) header gets replaced.
	*p32(b, 4) = 1
	initHeader(b, 2)
	if !ready(b) || channelCount(b) != 2 || chName(channel(b, 1)) != "" {
		t.Fatal("old layout not reset")
	}
}

func TestParseY4M(t *testing.T) {
	w, h, err := parseY4M("YUV4MPEG2 W1920 H1080 F30:1 Ip A1:1 C420jpeg XYSCSS=420JPEG\n")
	if err != nil || w != 1920 || h != 1080 {
		t.Fatal(w, h, err)
	}
	if _, _, err := parseY4M("YUV4MPEG2 W2 H2 C422\n"); err == nil {
		t.Fatal("422 accepted")
	}
	if _, _, err := parseY4M("garbage\n"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestParseMaxSize(t *testing.T) {
	out := `[in#0 @ 0000016b15a61ec0]   pixel_format=nv12  min s=640x480 fps=30 max s=1280x720 fps=30
[in#0 @ 0000016b15a61ec0]   pixel_format=yuyv422  min s=1920x1080 fps=30 max s=1920x1080 fps=30`
	if w, h, ok := parseMaxSize(out); !ok || w != 1920 || h != 1080 {
		t.Fatal(w, h, ok)
	}
	if _, _, ok := parseMaxSize("Error opening input"); ok {
		t.Fatal("parsed garbage")
	}
}
