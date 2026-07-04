# Multi-stage build for the local e2e stack: compiles every Engram binary once
# and ships a small runtime image. Each compose service selects its binary via
# `command`. Build context is the repo root (see docker-compose.yml).
FROM golang:1.25 AS build
WORKDIR /src
# Cache module downloads first.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/engram-server ./cmd/engram-server \
 && CGO_ENABLED=0 go build -o /out/engram-embed-server ./cmd/engram-embed-server \
 && CGO_ENABLED=0 go build -o /out/engram-stub-llm ./cmd/engram-stub-llm \
 && CGO_ENABLED=0 go build -o /out/engram ./cmd/engram \
 && CGO_ENABLED=0 go build -o /out/engram-mcp ./cmd/engram-mcp

FROM alpine:3.20
RUN apk add --no-cache curl ca-certificates
COPY --from=build /out/ /usr/local/bin/
# Default command; compose overrides per service.
CMD ["engram-server"]
