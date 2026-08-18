.PHONY: build build-token-stub test test-integration lint vet fmt mod-tidy docker docker-token-stub clean

BINARY := gcp-emulator-provisioner
TOKEN_STUB := gcp-token-stub

build:
	go build -o $(BINARY) ./cmd/gcp-emulator-provisioner

build-token-stub:
	go build -o $(TOKEN_STUB) ./cmd/gcp-token-stub

test:
	go test -race ./...

test-integration:
	go test -race -tags=integration -v -timeout 30m ./...

lint:
	go tool golangci-lint run

vet:
	go vet ./...

fmt:
	go fmt ./...

mod-tidy:
	go mod tidy

docker:
	docker build -t $(BINARY) .

docker-token-stub:
	docker build -f Dockerfile.token-stub -t $(TOKEN_STUB) .

clean:
	rm -f $(BINARY) $(TOKEN_STUB)
