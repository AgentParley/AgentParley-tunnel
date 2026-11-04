MODULE      := github.com/agentparley/tunnel
BINARY      := agentparley
PROTO_DIR   := proto
PROTO_OUT   := internal/proto
DIST_DIR    := dist

.PHONY: proto build build-linux-amd64 build-linux-arm64 build-darwin-arm64 build-darwin-amd64 build-windows-amd64 clean

# Regenerates internal/proto from proto/tunnel.proto — the SAME .proto the SshEgress project's C# server compiles
# server-side codegen from (AgentParley.Cloud.SshEgress.csproj's <Protobuf> item). Requires protoc,
# protoc-gen-go and protoc-gen-go-grpc on PATH (`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`
# and `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`).
proto:
	protoc \
		--go_out=$(PROTO_OUT) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_OUT) --go-grpc_opt=paths=source_relative \
		--proto_path=$(PROTO_DIR) $(PROTO_DIR)/tunnel.proto

# Local-platform build. The terminal commands (auth, chat, agents, chats) build on any OS; the daemon commands
# only on Linux (elsewhere they print a Linux-only message).
build:
	go build -o $(DIST_DIR)/$(BINARY) .

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o $(DIST_DIR)/$(BINARY)-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(DIST_DIR)/$(BINARY)-linux-arm64 .

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o $(DIST_DIR)/$(BINARY)-darwin-arm64 .

build-darwin-amd64:
	GOOS=darwin GOARCH=amd64 go build -o $(DIST_DIR)/$(BINARY)-darwin-amd64 .

build-windows-amd64:
	GOOS=windows GOARCH=amd64 go build -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe .

clean:
	rm -rf $(DIST_DIR)
