.PHONY: all test
all:
	go build -o virtcambridge .
	GOOS=windows go build -ldflags=-H=windowsgui -o virtcambridge.exe .

test:
	go test ./...
