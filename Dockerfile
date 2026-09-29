# syntax=docker/dockerfile:1

# dir2mcp in a container: serve a mounted folder as an MCP server.
#
#   docker build -t dir2mcp .
#   docker run --rm -p 127.0.0.1:8080:8080 -v "$PWD:/corpus:ro" -v dir2mcp-state:/state dir2mcp
#
# The server listens on 0.0.0.0:8080 inside the container with the default
# bearer-token auth. The token is /state/secret.token; read it with
#   docker run --rm -v dir2mcp-state:/state alpine cat /state/secret.token
# and send it as "Authorization: Bearer <token>" on http://localhost:8080/mcp.
#
# Config: a .dir2mcp.yaml in the mounted corpus wins; otherwise the image
# default (docker/config.yaml) binds embeddings and answers to an Ollama on
# the Docker host at host.docker.internal:11434. The server starts and serves
# its tools when that Ollama is not reachable; indexing then waits for it.

# --- build: static binary, no cgo (modernc sqlite is pure Go) -----------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/dirstral/dir2mcp/internal/buildinfo.Version=${VERSION}" \
    -o /out/dir2mcp ./cmd/dir2mcp

# --- runtime: alpine plus the tools the media path shells out to -------------
FROM alpine:3.21
# ffmpeg and ffprobe: audio and video duration, clips and time windows
# (internal/avutil). ca-certificates: TLS to cloud providers. tzdata: the
# date filters. No docling: use ingest.extractor=mistral or a docling-serve
# container for PDFs and images.
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
 && addgroup -S dir2mcp && adduser -S -G dir2mcp dir2mcp \
 && mkdir -p /corpus /state && chown dir2mcp:dir2mcp /corpus /state
COPY --from=build /out/dir2mcp /usr/local/bin/dir2mcp
COPY docker/config.yaml /etc/dir2mcp/config.yaml
COPY docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh
USER dir2mcp
WORKDIR /corpus
VOLUME ["/corpus", "/state"]
EXPOSE 8080
# The MCP registry reads this annotation to verify the image belongs to the
# server entry (docs/modelcontextprotocol-io/package-types.mdx, OCI).
LABEL io.modelcontextprotocol.server.name="io.github.dirstral/dir2mcp" \
      org.opencontainers.image.source="https://github.com/dirstral/dir2mcp" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.description="Serve any folder as an MCP knowledge server with cited answers"
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["up", "--foreground", "--non-interactive", "--dir", "/corpus", "--state-dir", "/state", "--listen", "0.0.0.0:8080"]
