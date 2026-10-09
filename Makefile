BINARY := dockerfast
CMD := ./cmd/dockerfast
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt lint run docker clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

test:
	go test ./... -race -count=1

vet:
	go vet ./...

fmt:
	gofmt -w .

run: build
	./bin/$(BINARY) -config config.example.json

docker:
	docker build -t dockerfast:$(VERSION) -t dockerfast:latest .

clean:
	rm -rf bin
