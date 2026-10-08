VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet integration proto clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/shoal ./cmd/shoal

test:
	go test ./...

vet:
	go vet ./...
	go vet -tags integration ./...

# Needs root, containerd 2.x, CNI plugins in /opt/cni/bin and network access.
# The test binary is built as the current user and only executed with sudo.
integration: build
	go test -c -tags integration -o bin/containerd.test ./internal/runtime/containerd
	sudo ./bin/containerd.test -test.v -test.count=1 -shoal-bin=$(abspath bin/shoal)

# Needs protoc, protoc-gen-go and protoc-gen-go-grpc on PATH. Generated code
# is checked in, so this is only needed after editing a .proto file.
proto:
	protoc -I . --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/v1alpha1/*.proto

clean:
	rm -rf bin
