// Host daemon: copies each guest camera's frames into its own v4l2loopback device.
// Devices are created (named after the guest camera) when a camera starts streaming and
// removed when it goes quiet, via the /dev/v4l2loopback control device.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	vidiocSFmt         = 3<<30 | 208<<16 | 'V'<<8 | 5 // _IOWR('V', 5, struct v4l2_format)
	bufTypeVideoOutput = 2
	pixFmtYU12         = 'Y' | 'U'<<8 | '1'<<16 | '2'<<24
	fieldNone          = 1
	loopCtlAdd         = 1<<30 | 72<<16 | '~'<<8 | 1 // _IOW('~', 1, struct v4l2_loopback_config)
	loopCtlRemove      = 1<<30 | 4<<16 | '~'<<8 | 2  // _IOW('~', 2, __u32); arg is the number itself
	idle               = 2 * time.Second
)

// struct v4l2_loopback_config (v4l2loopback.h)
type loopConf struct {
	outputNr, captureNr           int32
	label                         [32]byte
	minW, maxW, minH, maxH        uint32
	maxBuffers, maxOpeners, debug int32
	announceAllCaps               int32 // 0 = exclusive caps: apps see a camera only while we write
}

var _ [72]byte = [unsafe.Sizeof(loopConf{})]byte{} // ABI size check

// cam is the host side of one channel.
type cam struct {
	last             uint64
	name             string
	nr               int // /dev/video<nr>, -1 if none
	f                *os.File
	w, h             int
	lastFrame, retry time.Time
	buf              []byte
}

// Devices still held open by some app when we tried to remove them.
var pendingRemove []int

func main() {
	shmPath := flag.String("shm", "/dev/shm/virtcambridge", "ivshmem backing file")
	channels := flag.Int("channels", 4, "max guest cameras (only applied when the shared memory is (re)initialised)")
	flag.Parse()
	log.SetFlags(0) // journald timestamps

	// Devices a previous run couldn't remove (open at exit) stay hidden forever otherwise:
	// still listed by browsers, but unopenable, which they report as "permission denied".
	sysNames, _ := filepath.Glob("/sys/class/video4linux/video*/name")
	for _, p := range sysNames {
		var nr int
		if n, _ := os.ReadFile(p); strings.HasPrefix(string(n), "VM: ") {
			fmt.Sscanf(filepath.Base(filepath.Dir(p)), "video%d", &nr)
			hide(nr)
			pendingRemove = append(pendingRemove, nr)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)

	var (
		b       []byte
		ino     uint64
		cams    []*cam
		checked time.Time
		err     error
	)
	stopAll := func(why string) {
		for _, c := range cams {
			c.stop(why)
		}
		cams = nil
	}

	for {
		select {
		case <-sig:
			stopAll("shutting down")
			for _, nr := range pendingRemove {
				removeDevice(nr)
			}
			return
		default:
		}
		now := time.Now()
		if now.Sub(checked) >= time.Second {
			checked = now
			retryPending()
			// QEMU (re)creates the backing file when the VM starts; follow it.
			var st syscall.Stat_t
			serr := syscall.Stat(*shmPath, &st)
			if b != nil && (serr != nil || st.Ino != ino || int(st.Size) != len(b)) {
				stopAll("shared memory went away")
				syscall.Munmap(b)
				b = nil
			}
			if b == nil && serr == nil {
				if b, err = mapShm(*shmPath, int(st.Size), *channels); err != nil {
					log.Print(err)
				} else {
					ino = st.Ino
					for i := range channelCount(b) {
						// start from the current frame, not a stale one
						cams = append(cams, &cam{nr: -1, last: atomic.LoadUint64(p64(channel(b, i), offLatest))})
					}
					log.Printf("attached to %s (%d MiB, %d channels)", *shmPath, len(b)>>20, len(cams))
				}
			}
		}
		if b == nil {
			time.Sleep(time.Second)
			continue
		}

		busy := false
		for i, c := range cams {
			busy = c.poll(channel(b, i), now) || busy
		}
		if !busy {
			time.Sleep(4 * time.Millisecond) // ponytail: polling, ivshmem-plain has no doorbell; ivshmem-doorbell if latency matters
		}
	}
}

func (c *cam) poll(ch []byte, now time.Time) bool {
	var w, h int
	var frame []byte
	c.last, w, h, frame = readFrame(ch, c.last, c.buf)
	if frame == nil {
		if c.nr >= 0 && now.Sub(c.lastFrame) > idle {
			c.stop("no frames from guest")
		}
		return false
	}
	c.buf, c.lastFrame = frame, now
	name := chName(ch)
	if c.nr >= 0 && (name != c.name || w != c.w || h != c.h) {
		c.stop("camera changed")
	}
	if c.nr < 0 && now.After(c.retry) {
		if err := c.start(name, w, h); err != nil {
			log.Printf("%q: %v", name, err)
			c.retry = now.Add(5 * time.Second)
		}
	}
	if c.f != nil {
		if _, err := c.f.Write(frame); err != nil {
			c.stop(err.Error())
		}
	}
	return true
}

func (c *cam) start(name string, w, h int) error {
	conf := loopConf{outputNr: -1, captureNr: -1}
	copy(conf.label[:31], "VM: "+name)
	nr, err := loopCtl(loopCtlAdd, uintptr(unsafe.Pointer(&conf)))
	if err != nil {
		return fmt.Errorf("create device: %w", err)
	}
	f, err := openOutput(fmt.Sprintf("/dev/video%d", nr), w, h)
	if err != nil {
		removeDevice(nr)
		return err
	}
	c.name, c.nr, c.f, c.w, c.h = name, nr, f, w, h
	log.Printf("%q: streaming %dx%d to /dev/video%d", name, w, h, nr)
	return nil
}

func (c *cam) stop(why string) {
	if c.nr < 0 {
		return
	}
	log.Printf("%q: closed /dev/video%d: %s", c.name, c.nr, why)
	c.f.Close()
	if err := removeDevice(c.nr); errors.Is(err, syscall.EBUSY) {
		hide(c.nr)
		pendingRemove = append(pendingRemove, c.nr) // an app still has it open
	}
	c.nr, c.f = -1, nil
}

// hide drops a device an app still holds open (e.g. a reader stuck waiting for frames)
// out of camera lists until it can be removed: nobody else can open it, and the change
// event makes PipeWire re-probe it and forget it.
func hide(nr int) {
	os.Chmod(fmt.Sprintf("/dev/video%d", nr), 0)
	os.WriteFile(fmt.Sprintf("/sys/class/video4linux/video%d/uevent", nr), []byte("change"), 0)
}

func retryPending() {
	keep := pendingRemove[:0]
	for _, nr := range pendingRemove {
		if errors.Is(removeDevice(nr), syscall.EBUSY) {
			keep = append(keep, nr)
		}
	}
	pendingRemove = keep
}

func removeDevice(nr int) error {
	_, err := loopCtl(loopCtlRemove, uintptr(nr))
	return err
}

func loopCtl(cmd, arg uintptr) (int, error) {
	f, err := os.OpenFile("/dev/v4l2loopback", os.O_RDWR, 0)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cmd, arg)
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}

func mapShm(path string, size, channels int) ([]byte, error) {
	if channels < 1 || channels > maxChannels || (size-hdrSize)/channels < hdrSize+nSlots*4096 {
		return nil, fmt.Errorf("%s: %d bytes is too small for %d channels", path, size, channels)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	initHeader(b, channels)
	return b, nil
}

func openOutput(path string, w, h int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var vf [208]byte // struct v4l2_format; pix format starts at offset 8
	le := binary.LittleEndian
	le.PutUint32(vf[0:], bufTypeVideoOutput)
	le.PutUint32(vf[8:], uint32(w))
	le.PutUint32(vf[12:], uint32(h))
	le.PutUint32(vf[16:], pixFmtYU12)
	le.PutUint32(vf[20:], fieldNone)
	le.PutUint32(vf[24:], uint32(w))              // bytesperline
	le.PutUint32(vf[28:], uint32(i420Size(w, h))) // sizeimage
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), vidiocSFmt, uintptr(unsafe.Pointer(&vf[0]))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("%s: set format %dx%d: %w", path, w, h, e)
	}
	return f, nil
}
