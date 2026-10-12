.PHONY: build test check run demo
build:
	go build -trimpath -o bin/julia ./cmd/julia
test:
	go test -race ./...
check:
	go vet ./...
	go test -race ./...
run:
	go run ./cmd/julia
demo:
	go run ./cmd/julia --demo
