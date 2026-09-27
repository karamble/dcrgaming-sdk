GO ?= go

.PHONY: check
check:
	$(GO) test -race ./...
	$(GO) vet ./...
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Files need 'gofmt -s -w .':" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

.PHONY: test
test:
	$(GO) test ./...

# This module generates the bridge contract's stubs. The plugins are built at
# the pinned versions for the run, and the proto path is the gamingpb
# directory itself, which keeps the descriptor's recorded path unchanged.
.PHONY: proto
proto:
	@command -v protoc >/dev/null || { echo "protoc not found" >&2; exit 1; }
	@bin=$$(mktemp -d) && trap 'rm -rf "$$bin"' EXIT && \
	GOBIN=$$bin $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 && \
	GOBIN=$$bin $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2 && \
	cd pkg/gaming/gamingpb && protoc --proto_path=. \
		--plugin=protoc-gen-go=$$bin/protoc-gen-go --go_out=. --go_opt=paths=source_relative \
		--plugin=protoc-gen-go-grpc=$$bin/protoc-gen-go-grpc --go-grpc_out=. --go-grpc_opt=paths=source_relative \
		gaming_bridge.proto

# The example game is the shortest complete integration. It runs against
# pkg/gaming/bridgetest, so it needs no wallet, bridge or network.
.PHONY: example
example:
	$(GO) run ./example
