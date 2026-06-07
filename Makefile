COVERAGE_MIN ?= 85.0

.PHONY: build test cover mocks lint fmt vet tidy

build:
	go build -o bin/cerber ./cmd/cerber

# Unit tests + coverage gate. Excludes integration-tagged tests.
test: cover
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "total coverage: $$total% (min $(COVERAGE_MIN)%)"; \
	awk "BEGIN { exit !($$total >= $(COVERAGE_MIN)) }" || { echo "FAIL: coverage below $(COVERAGE_MIN)%"; exit 1; }

cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...

# Per-package coverage breakdown.
cover-report: cover
	go tool cover -func=coverage.out

mocks:
	mockery

lint: fmt vet

fmt:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './*/mocks/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

tidy:
	go mod tidy
