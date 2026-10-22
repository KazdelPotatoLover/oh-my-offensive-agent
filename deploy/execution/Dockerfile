# Build with the backend module as the context:
#   docker build -f deploy/execution/Dockerfile -t oma-executor:ma4 backend
FROM golang:1.26-bookworm AS proxy
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /oma-proxy ./cmd/oma-proxy

FROM python:3.12-slim-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/local/bin/python3 /usr/local/bin/python \
    && useradd --uid 10001 --create-home --shell /usr/sbin/nologin oma
COPY --from=proxy /oma-proxy /usr/local/bin/oma-proxy
RUN chmod 755 /usr/local/bin/oma-proxy
USER 10001
WORKDIR /workspace
