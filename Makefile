.PHONY: build test check

build:
	go build -o build/ciderload ./cmd/ciderload
	go build -o build/cidl ./cmd/cidl

test:
	go test -race ./...

check:
	go vet ./...
