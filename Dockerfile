# Local build: docker build -t llmproxy .
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/wjzhangq/llmproxy/internal/version.Version=${VERSION}" \
    -o /out/llmproxy ./cmd/llmproxy

FROM busybox:1.36 AS dirs
RUN mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=dirs --chown=65532:65532 /data /data
COPY --from=build /out/llmproxy /usr/local/bin/llmproxy
WORKDIR /data
ENV LLMPROXY_LOG_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/llmproxy"]
