# Build stage
FROM golang:latest AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o server ./cmd/server

# Runtime stage (matching ARTEX runtime with Node, Playwright MCP, and Chromium)
FROM debian:bookworm-slim
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=0
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright install --with-deps chromium \
    && mkdir -p /ms-playwright && chmod -R 755 /ms-playwright \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /build/server .
COPY --from=builder /build/migrations ./migrations
EXPOSE 8080
CMD ["./server"]
