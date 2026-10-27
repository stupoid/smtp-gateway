# Local gates. CI runs the same steps on Linux.
#
#   make check    format, vet, lint, race tests
#   make test     race tests only
#   make e2e      shell suites; needs swaks, mutt, msmtp, nc
#   make examples build the example modules (separate Go modules)

GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: check fmt fmt-check vet lint test cover e2e examples

check: fmt-check vet lint test

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed:"; \
		echo "$$out"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

lint:
	@command -v $(GOLANGCI_LINT) >/dev/null || { \
		echo "$(GOLANGCI_LINT) not found: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run ./...

test:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

e2e:
	@for s in e2e/run-e2e*.sh; do \
		echo "== $$s"; \
		"$$s"; \
	done

examples:
	cd examples/postcat-only && $(GO) build ./...
	cd examples/s3 && $(GO) build ./...
