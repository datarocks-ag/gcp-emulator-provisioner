.PHONY: build test test-integration lint vet fmt mod-tidy docker clean

BINARY := gcp-emulator-provisioner

build:
	go build -o $(BINARY) ./cmd/gcp-emulator-provisioner

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

clean:
	rm -f $(BINARY)
