COVERAGE_MIN ?= 85.0

.PHONY: build test cover mocks lint fmt vet tidy hooks

# Install git hooks (commit-msg). Pins a local hooksPath to defeat a global one.
hooks:
	@hd="$$(git rev-parse --absolute-git-dir)/hooks"; \
	git config --local core.hooksPath "$$hd"; \
	ln -sf "$$(pwd)/.githooks/commit-msg" "$$hd/commit-msg"; \
	echo "installed commit-msg -> $$hd"

build:
	go build -o bin/cerber ./cmd/cerber

# Unit tests + coverage gate. Excludes integration-tagged tests and the thin
# cmd/ wiring layer (which is exercised by integration/e2e, not unit tests).
test: cover
	@grep -vE '^cerber/cmd/|/mocks/' coverage.out > coverage.gate.out; \
	total=$$(go tool cover -func=coverage.gate.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "total coverage: $$total% (min $(COVERAGE_MIN)%)"; \
	awk "BEGIN { exit !($$total >= $(COVERAGE_MIN)) }" || { echo "FAIL: coverage below $(COVERAGE_MIN)%"; exit 1; }

cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...

# Per-package coverage breakdown.
cover-report: cover
	go tool cover -func=coverage.out

# Regenerate mocks. No-op until .mockery.yaml has uncommented package entries
# (mockery v2 panics on an empty packages map).
mocks:
	@if grep -qE '^  cerber/' .mockery.yaml; then mockery; else echo "no packages configured in .mockery.yaml yet — skipping"; fi

lint: fmt vet

fmt:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './*/mocks/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

tidy:
	go mod tidy
