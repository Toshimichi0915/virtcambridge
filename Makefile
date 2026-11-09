.PHONY: all test install
all:
	go build -o virtcambridge .
	GOOS=windows go build -ldflags=-H=windowsgui -o virtcambridge.exe .

test:
	go test ./...

install:
	install -Dm755 virtcambridge /usr/local/bin/virtcambridge
	install -Dm644 virtcambridge.service /etc/systemd/system/virtcambridge.service
	install -Dm644 v4l2loopback.conf /etc/modprobe.d/virtcambridge.conf
	systemctl daemon-reload
	systemctl enable --now virtcambridge
