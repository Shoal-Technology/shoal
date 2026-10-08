VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race vet integration integration-report proto clean

race:
	go test -race -count=1 -timeout=5m ./...

# Run only on disposable Linux hosts: this suite changes host networking.
# Preserve JSON evidence and reject skipped tests instead of false-green runs.
integration-report: build
	go test -c -tags integration -o bin/containerd.test ./internal/runtime/containerd
	mkdir -p test-results
	bash -o pipefail -c 'sudo ./bin/containerd.test -test.v -test.count=1 -test.timeout=10m -shoal-bin=$(abspath bin/shoal) 2>&1 | go tool test2json -t -p example.com/shoal/internal/runtime/containerd | tee test-results/integration.jsonl; run_status=$$?; python3 hack/test_report.py --require-no-skips test-results/integration.jsonl; report_status=$$?; test $$run_status -eq 0 && test $$report_status -eq 0'

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
