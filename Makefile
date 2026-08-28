MODULE      := github.com/agentparley/tunnel
BINARY      := agentparley-tunnel
PROTO_DIR   := proto
PROTO_OUT   := internal/proto
DIST_DIR    := dist

.PHONY: proto build build-linux-amd64 build-linux-arm64 clean

# Regenerates internal/proto from proto/tunnel.proto — the SAME .proto the SshEgress project's C# server compiles
# server-side codegen from (AgentParley.Cloud.SshEgress.csproj's <Protobuf> item). Requires protoc,
# protoc-gen-go and protoc-gen-go-grpc on PATH (`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`
# and `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`).
proto:
	protoc \
		--go_out=$(PROTO_OUT) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_OUT) --go-grpc_opt=paths=source_relative \
		--proto_path=$(PROTO_DIR) $(PROTO_DIR)/tunnel.proto

# Local-platform build. Only succeeds when GOOS=linux (the host's default on a Linux dev box) — this daemon is
# Linux-only by design (no *_darwin.go, no Windows; see the plan's guardrails), so building on a non-Linux
# workstation needs one of the cross-compile targets below instead.
build:
	go build -o $(DIST_DIR)/$(BINARY) .

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o $(DIST_DIR)/$(BINARY)-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(DIST_DIR)/$(BINARY)-linux-arm64 .

clean:
	rm -rf $(DIST_DIR)
