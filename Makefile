.PHONY: build test check run demo
build:
	go build -trimpath -o bin/jarvis ./cmd/jarvis
test:
	go test -race ./...
check:
	go vet ./...
	go test -race ./...
run:
	go run ./cmd/jarvis
demo:
	go run ./cmd/jarvis --demo
