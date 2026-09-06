# Builds both binaries; the image runs the server by default and the agent
# when the entrypoint is overridden (see docker-compose.yml).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/surfswarm-server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/surfswarm-agent ./cmd/agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -h /var/lib/surfswarm surfswarm \
 && mkdir -p /var/lib/surfswarm/data /var/lib/surfswarm/state \
 && chown -R surfswarm:surfswarm /var/lib/surfswarm
COPY --from=build /out/surfswarm-server /out/surfswarm-agent /usr/local/bin/
USER surfswarm
WORKDIR /var/lib/surfswarm
ENV SURFSWARM_DATA=/var/lib/surfswarm/data \
    SURFSWARM_STATE_DIR=/var/lib/surfswarm/state
VOLUME ["/var/lib/surfswarm/data"]
EXPOSE 8080
ENTRYPOINT ["surfswarm-server"]
