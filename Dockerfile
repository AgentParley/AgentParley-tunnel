# Dev-only image for local end-to-end testing of the tunnel daemon (docker-compose.dev.yml's tunnel-test-host).
# The container starts idle — `agentparley-tunnel login` is an interactive device-grant approval, so a developer
# runs it by hand:
#   docker compose -f infrastructure/docker-compose.dev.yml exec tunnel-test-host agentparley-tunnel login
#   docker compose -f infrastructure/docker-compose.dev.yml exec -d tunnel-test-host agentparley-tunnel start
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY . .
RUN make build-linux-amd64

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/dist/agentparley-tunnel-linux-amd64 /usr/local/bin/agentparley-tunnel
RUN mkdir -p /etc/agentparley-tunnel /var/lib/agentparley-tunnel
COPY packaging/dev/config.yaml /etc/agentparley-tunnel/config.yaml
CMD ["sleep", "infinity"]
