GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build runner-binary test race vet check clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ ./cmd/raccoonci-runner

# The binary the InstaBox runner snapshot installs: static, so it runs on any
# template. Build it with a real VERSION; `raccoonci-runner runner-snapshot` refuses "dev".
runner-binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/raccoonci-runner-linux-amd64 ./cmd/raccoonci-runner

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# Everything that must pass before merging.
check: vet race

clean:
	rm -rf dist
