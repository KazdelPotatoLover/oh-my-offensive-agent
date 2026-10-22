FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /oma-broker ./cmd/oma-broker

FROM debian:bookworm-slim
# This process is the only one that opens the Docker socket. The host socket is
# typically root:docker mode 660, so the image does not drop to uid 10001.
# Execution containers are still forced to 10001:10001 by the broker.
# Do not reuse this image or this user for the API service.
COPY --from=build /oma-broker /oma-broker
ENTRYPOINT ["/oma-broker"]
