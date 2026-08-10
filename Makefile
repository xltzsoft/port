BINS = port-server port-client

.PHONY: all build clean

all: build

build:
	go build -trimpath -ldflags "-s -w" -o port-server ./cmd/server
	go build -trimpath -ldflags "-s -w" -o port-client ./cmd/client

clean:
	rm -f $(BINS)
