// Guest capture: streams every DirectShow virtual camera through ffmpeg into its own
// ivshmem channel. Cameras are rescanned every 10 s and each capture retries forever, so
// a camera reaches the host whenever it exists and produces frames.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// virtio-win IVSHMEM driver interface (the one Looking Glass uses).
var (
	guidIvshmem = syscall.GUID{Data1: 0xdf576976, Data2: 0x569d, Data3: 0x4672, Data4: [8]byte{0x95, 0xa0, 0xf5, 0x7e, 0x4e, 0xa0, 0xb2, 0x10}}
	cfgmgr32    = syscall.NewLazyDLL("cfgmgr32.dll")
	cmListSize  = cfgmgr32.NewProc("CM_Get_Device_Interface_List_SizeW")
	cmList      = cfgmgr32.NewProc("CM_Get_Device_Interface_ListW")
)

const (
	ioctlRequestMmap   = 0x222008 // CTL_CODE(FILE_DEVICE_UNKNOWN, 0x802, METHOD_BUFFERED, FILE_ANY_ACCESS)
	ioctlReleaseMmap   = 0x22200c // ... 0x803 ...
	cacheWriteCombined = 2
	createNoWindow     = 0x08000000
	// Registered DirectShow video sources (CLSID_VideoInputDeviceCategory). Software
	// virtual cameras (OBS, ...) register here; physical (KS) cameras don't, so they're
	// never grabbed away from guest apps.
	dshowVideoSources = `CLSID\{860BB310-5D01-11D0-BD3B-00A0C911CE86}\Instance`
)

type ivshmemMmap struct {
	peerID  uint16
	size    uint64
	ptr     unsafe.Pointer
	vectors uint16
}

type stream struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func main() {
	ffmpeg := flag.String("ffmpeg", defaultFFmpeg(), "path to ffmpeg.exe")
	flag.Parse()
	if f, err := os.Create(filepath.Join(os.TempDir(), "virtcambridge.log")); err == nil {
		log.SetOutput(f) // built with -H=windowsgui: no console
	}

	log.Print("looking for the virtcambridge ivshmem device")
	b := openShm()
	for ; b == nil; b = openShm() {
		time.Sleep(2 * time.Second)
	}
	n := channelCount(b)
	log.Printf("attached to ivshmem (%d MiB, %d channels)", len(b)>>20, n)

	used := make([]string, n) // channel -> camera
	streams := map[string]stream{}
	full := map[string]bool{} // cameras already reported as not fitting
	for {
		cams := virtualCameras()
		for i, name := range used {
			if name != "" && !slices.Contains(cams, name) {
				log.Printf("%q: removed", name)
				streams[name].cancel()
				<-streams[name].done
				delete(streams, name)
				used[i] = ""
			}
		}
		for _, name := range cams {
			if _, ok := streams[name]; ok {
				continue
			}
			i := slices.Index(used, "")
			if i < 0 {
				if !full[name] {
					log.Printf("%q: all %d channels in use; raise -channels on the host", name, n)
					full[name] = true
				}
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			s := stream{cancel, make(chan struct{})}
			streams[name], used[i] = s, name
			go run(ctx, *ffmpeg, name, channel(b, i), s.done)
		}
		time.Sleep(10 * time.Second)
	}
}

func run(ctx context.Context, ffmpeg, camera string, ch []byte, done chan struct{}) {
	defer close(done)
	setName(ch, camera)
	defer setName(ch, "")
	w := newWriter(ch)
	lastErr := ""
	for ctx.Err() == nil {
		streamed, err := capture(ctx, ffmpeg, camera, w)
		if streamed {
			lastErr = ""
		}
		if msg := err.Error(); msg != lastErr && ctx.Err() == nil {
			log.Print(msg)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func defaultFFmpeg() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "ffmpeg.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "ffmpeg.exe"
}

func virtualCameras() []string {
	var k syscall.Handle
	path, _ := syscall.UTF16PtrFromString(dshowVideoSources)
	if syscall.RegOpenKeyEx(syscall.HKEY_CLASSES_ROOT, path, 0, syscall.KEY_READ, &k) != nil {
		return nil
	}
	defer syscall.RegCloseKey(k)
	friendly, _ := syscall.UTF16PtrFromString("FriendlyName")
	var names []string
	for i := uint32(0); ; i++ {
		var sub [256]uint16
		n := uint32(len(sub))
		if syscall.RegEnumKeyEx(k, i, &sub[0], &n, nil, nil, nil, nil) != nil {
			break
		}
		var sk syscall.Handle
		if syscall.RegOpenKeyEx(k, &sub[0], 0, syscall.KEY_READ, &sk) != nil {
			continue
		}
		var val [256]uint16
		var typ uint32
		vn := uint32(len(val) * 2)
		if syscall.RegQueryValueEx(sk, friendly, nil, &typ, (*byte)(unsafe.Pointer(&val[0])), &vn) == nil && typ == syscall.REG_SZ {
			names = append(names, syscall.UTF16ToString(val[:]))
		}
		syscall.RegCloseKey(sk)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func ivshmemPaths() []string {
	var n uint32
	if r, _, _ := cmListSize.Call(uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&guidIvshmem)), 0, 0); r != 0 || n < 2 {
		return nil
	}
	buf := make([]uint16, n)
	if r, _, _ := cmList.Call(uintptr(unsafe.Pointer(&guidIvshmem)), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(n), 0); r != 0 {
		return nil
	}
	var paths []string
	for start, i := 0, 0; i < len(buf); i++ { // REG_MULTI_SZ
		if buf[i] == 0 {
			if i == start {
				break
			}
			paths = append(paths, syscall.UTF16ToString(buf[start:i]))
			start = i + 1
		}
	}
	return paths
}

// openShm maps the first ivshmem device the host daemon has stamped with our magic.
// Others are released immediately; one held by Looking Glass fails to open and is skipped.
func openShm() []byte {
	for _, p := range ivshmemPaths() {
		path, _ := syscall.UTF16PtrFromString(p)
		h, err := syscall.CreateFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		cfg := byte(cacheWriteCombined)
		var m ivshmemMmap
		var n uint32
		if syscall.DeviceIoControl(h, ioctlRequestMmap, &cfg, 1, (*byte)(unsafe.Pointer(&m)), uint32(unsafe.Sizeof(m)), &n, nil) == nil {
			if b := unsafe.Slice((*byte)(m.ptr), m.size); ready(b) {
				return b // handle stays open for the life of the process
			}
			syscall.DeviceIoControl(h, ioctlReleaseMmap, nil, 0, nil, 0, &n, nil)
		}
		syscall.CloseHandle(h)
	}
	return nil
}

// cameraSize asks the camera for its current mode without streaming from it.
func cameraSize(ctx context.Context, ffmpeg, camera string) (w, h int, ok bool) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-f", "dshow", "-list_options", "true", "-i", "video="+camera)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, _ := cmd.CombinedOutput() // always exits non-zero with -list_options
	return parseMaxSize(string(out))
}

func capture(ctx context.Context, ffmpeg, camera string, w *writer) (streamed bool, err error) {
	ctx, cancel := context.WithCancel(ctx) // also ends the resize watcher below
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "dshow", "-rtbufsize", "64M"}
	// Ask for the camera's largest mode explicitly; ffmpeg's default pick may be smaller.
	if mw, mh, ok := cameraSize(ctx, ffmpeg, camera); ok {
		args = append(args, "-video_size", fmt.Sprintf("%dx%d", mw, mh))
	}
	args = append(args, "-i", "video="+camera, "-pix_fmt", "yuv420p", "-f", "yuv4mpegpipe", "pipe:1")
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%v: %s", err, msg[strings.LastIndex(msg, "\n")+1:])
		}
	}()

	r := bufio.NewReaderSize(out, 1<<20)
	line, err := r.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("%q: unavailable", camera)
	}
	width, height, err := parseY4M(line)
	if err != nil {
		return false, err
	}
	frame := make([]byte, i420Size(width, height))
	log.Printf("%q: streaming %dx%d", camera, width, height)

	// The format is fixed when ffmpeg connects, and virtual cameras (YoloCam, OBS) scale
	// their output down to it. If the app's output size changes later (or it wasn't
	// running yet when we connected), reconnect to get the camera's real resolution.
	var resized atomic.Value
	go func() {
		t := time.NewTicker(10 * time.Second) // ponytail: polls with an ffmpeg per camera (~0.3 s); fine for a few cameras
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if w, h, ok := cameraSize(ctx, ffmpeg, camera); ok && (w != width || h != height) {
					resized.Store(fmt.Sprintf("%dx%d", w, h))
					cmd.Process.Kill()
					return
				}
			}
		}
	}()
	stopped := func() error {
		if s, ok := resized.Load().(string); ok {
			return fmt.Errorf("%q: resolution changed to %s, reconnecting", camera, s)
		}
		return fmt.Errorf("%q: stopped", camera)
	}

	for {
		if line, err = r.ReadString('\n'); err != nil {
			return true, stopped()
		}
		if !strings.HasPrefix(line, "FRAME") {
			return true, fmt.Errorf("%q: bad y4m frame header %q", camera, line)
		}
		if _, err = io.ReadFull(r, frame); err != nil {
			return true, stopped()
		}
		if !w.put(width, height, frame) {
			return true, fmt.Errorf("%q: %dx%d doesn't fit in shared memory; make it bigger", camera, width, height)
		}
	}
}
