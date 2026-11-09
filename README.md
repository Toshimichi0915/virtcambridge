# virtcambridge

Exposes every virtual camera inside a Windows VM (OBS Virtual Camera, ...) on the
Linux host, over ivshmem (no network). Guest: one ffmpeg per DirectShow virtual camera
writes raw I420 frames into its own shared-memory channel. Host: a systemd daemon
creates a v4l2loopback device named `VM: <camera name>` when a camera starts streaming
and removes it 2 s after it stops.

Physical cameras in the guest are left alone (they aren't registered DirectShow filters).

## Host

```sh
sudo pacman -S --needed v4l2loopback-dkms linux-headers   # headers must match `uname -r`
make && sudo make install
```

Add a second ivshmem device to the VM (`virsh edit win11`, inside `<qemu:commandline>`,
next to the Looking Glass one), then restart the VM:

```xml
<qemu:arg value='-device'/>
<qemu:arg value='{"driver":"ivshmem-plain","id":"shmem1","memdev":"virtcambridge"}'/>
<qemu:arg value='-object'/>
<qemu:arg value='{"qom-type":"memory-backend-file","id":"virtcambridge","mem-path":"/dev/shm/virtcambridge","size":134217728,"share":true}'/>
```

128 MiB = 4 cameras up to ~2.5K each (daemon flag `-channels`, default 4). For 4K, use
32 MiB × channels × 2. When changing the size, `rm /dev/shm/virtcambridge` with the VM
off first: QEMU won't resize an existing file.

Logs: `journalctl -u virtcambridge -f`. Devices: `v4l2-ctl --list-devices`.

## Guest

Needs the IVSHMEM driver (already installed for Looking Glass). The installer finds
ffmpeg (next to it, or on PATH) or installs it with winget, and copies it into the
install folder. Copy `virtcambridge.exe`, `install.ps1` and `install.cmd` to the guest, then
double-click `install.cmd` (it asks for admin; no execution-policy change needed).
Uninstall from Settings > Apps > virtcambridge.

Log: `%TEMP%\virtcambridge.log`.
