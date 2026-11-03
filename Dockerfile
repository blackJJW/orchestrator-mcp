FROM golang:1.27.0-bookworm AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/orchestrator-mcp-server/ ./cmd/orchestrator-mcp-server/
COPY internal/ ./internal/
RUN go build -mod=readonly -trimpath -o /out/orchestrator-mcp-server ./cmd/orchestrator-mcp-server

FROM debian:bookworm-slim
# S2 invokes this exact path; no SSH client, supervisor or provider is installed.
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/orchestrator-mcp-server /usr/local/bin/orchestrator-mcp-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orchestrator-mcp-server"]
