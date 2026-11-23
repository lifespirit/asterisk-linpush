GO ?= go
PREFIX ?= /usr/local
VERSION ?= 0.4.0

.PHONY: build test vet clean
build:
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags='-s -w' -o linpush ./cmd/linpush

test:
	CGO_ENABLED=1 $(GO) test ./...

vet:
	$(GO) vet ./...

clean:
	rm -f linpush
