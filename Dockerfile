FROM golang:1.26-alpine@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5 AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY . .

# Get git commit hash and build date
RUN GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown") && \
    BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) && \
    CGO_ENABLED=0 go build -ldflags "-X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}" -o filebrowser .

# Seed an empty directory to mount the partial-upload store at. The app runs as
# a non-root user with a read-only container root, so it cannot create
# /.uploads (a sibling of the files dir) itself; ship it pre-created and owned
# by the runtime uid.
RUN mkdir -p /seed-uploads

FROM scratch
COPY --from=builder /src/filebrowser /filebrowser
COPY --from=builder --chown=1000:1000 /seed-uploads /.uploads
EXPOSE 8000

USER 1000:1000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/filebrowser", "health"]
ENTRYPOINT ["/filebrowser"]
