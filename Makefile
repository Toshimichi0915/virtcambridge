.PHONY: all test setup
all:
	go build -o virtcambridge .
	GOOS=windows go build -ldflags=-H=windowsgui -o virtcambridge.exe .

test:
	go test ./...

# Drop ffmpeg.exe next to virtcambridge.iss first to bundle it.
setup: all
	docker run --rm -u $$(id -u):$$(id -g) -v "$$PWD":/work amake/innosetup \
		/DAppVersion=r$$(git rev-list --count HEAD).$$(git rev-parse --short HEAD) virtcambridge.iss
