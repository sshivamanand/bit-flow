VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/arm64 darwin/amd64 windows/amd64 windows/arm64 linux/amd64 linux/arm64

.PHONY: build test bench e2e cross clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/bitflow ./cmd/bitflow

test:
	go test -race ./...

bench: build
	./bin/bitflow bench --loopback --sweep --size 10GB

# Drives the real binary through every user-facing flow (macOS).
e2e: build
	scripts/e2e-macos.sh $(CURDIR)/bin/bitflow $${TMPDIR:-/tmp}/bitflow-e2e

# Static binaries for every platform, ready to hand to a friend.
cross:
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "dist/bitflow-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/bitflow-$$os-$$arch$$ext ./cmd/bitflow || exit 1; \
	done

clean:
	rm -rf bin dist
