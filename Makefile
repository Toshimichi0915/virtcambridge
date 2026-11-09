.PHONY: all test install
all:
	go build -o videobridge .
	GOOS=windows go build -ldflags=-H=windowsgui -o videobridge.exe .

test:
	go test ./...

install:
	install -Dm755 videobridge /usr/local/bin/videobridge
	install -Dm644 videobridge.service /etc/systemd/system/videobridge.service
	install -Dm644 v4l2loopback.conf /etc/modprobe.d/videobridge.conf
	systemctl daemon-reload
	systemctl enable --now videobridge
