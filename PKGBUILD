# Builds the host daemon from this checkout: makepkg -si
pkgname=virtcambridge
pkgver=r4.2a3d609
pkgrel=1
pkgdesc="Expose a Windows VM's virtual cameras on the host via ivshmem and v4l2loopback"
arch=('x86_64')
license=('MIT')
depends=('v4l2loopback-dkms')
makedepends=('go' 'git')

pkgver() {
  cd "$startdir"
  printf "r%s.%s" "$(git rev-list --count HEAD)" "$(git rev-parse --short HEAD)"
}

build() {
  cd "$startdir"
  export CGO_ENABLED=0 GOFLAGS="-trimpath -mod=readonly -modcacherw"
  go build -o "$srcdir/virtcambridge" .
}

check() {
  cd "$startdir"
  go test .
}

package() {
  install -Dm755 "$srcdir/virtcambridge" "$pkgdir/usr/bin/virtcambridge"
  install -Dm644 "$startdir/virtcambridge.service" "$pkgdir/usr/lib/systemd/system/virtcambridge.service"
  install -Dm644 "$startdir/v4l2loopback.conf" "$pkgdir/usr/lib/modprobe.d/virtcambridge.conf"
  install -Dm644 "$startdir/LICENSE" "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
}
