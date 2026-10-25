FROM rust:alpine@sha256:557a94c82b485a14b569120e00867267108c734efe33f10cc31ca0cd5dd1eca9 AS builder
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY Cargo.toml Cargo.lock build.rs ./
COPY src ./src
COPY templates ./templates
ARG GIT_COMMIT=unknown
RUN cargo build --release --locked
RUN mkdir -p /seed-files /seed-uploads

FROM scratch
COPY --from=builder --chmod=0555 /src/target/release/filebrowser /filebrowser
COPY --from=builder --chown=1000:1000 --chmod=0700 /seed-files /files
COPY --from=builder --chown=1000:1000 --chmod=0700 /seed-uploads /.uploads
WORKDIR /
EXPOSE 8000
USER 1000:1000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/filebrowser", "health"]
ENTRYPOINT ["/filebrowser"]
