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

# The example game is the shortest complete integration. It runs against
# pkg/gaming/bridgetest, so it needs no wallet, bridge or network.
.PHONY: example
example:
	$(GO) run ./example
