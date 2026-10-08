BIN := bin/mux-scale
GO_FILES := $(shell find . -name '*.go' -not -path './bin/*')

.PHONY: build test lint clean

build: $(BIN)

$(BIN): go.mod go.sum $(GO_FILES)
	CGO_ENABLED=0 go build -o $(BIN) ./cmd/module

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipping"; fi

module.tar.gz: $(BIN) meta.json
	tar czf $@ $(BIN) meta.json

clean:
	rm -rf bin module.tar.gz
