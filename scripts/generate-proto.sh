#!/usr/bin/env sh
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

# Install protoc 33.5 from the official protobuf release for your OS. Source
# generation is optional: the official generated Go files are checked in.
if [ "$(protoc --version)" != 'libprotoc 33.5' ]; then
    echo 'protoc 33.5 is required' >&2
    exit 1
fi
mkdir -p .tools/bin api/gen
GOBIN="$(pwd)/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="$(pwd)/.tools/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.0
protoc --proto_path=api/proto \
    --plugin=protoc-gen-go=.tools/bin/protoc-gen-go \
    --plugin=protoc-gen-go-grpc=.tools/bin/protoc-gen-go-grpc \
    --go_out=api/gen --go_opt=paths=source_relative \
    --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
    api/proto/infrapulse/v1/agent.proto
