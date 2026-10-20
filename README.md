# Build

Requires Linux and the latest stable Rust toolchain.

```sh
cargo build --release --locked
./target/release/filebrowser
```

# Installation

Create the data directory writable by the container's UID/GID 1000:

```sh
sudo install -d -o 1000 -g 1000 /data/bin
```

Save as `compose.yaml`:

```yaml
services:
  filebrowser:
    image: ghcr.io/valonsodev/filebrowser:latest
    ports:
      - "7667:8000"
    volumes:
      - /data/bin:/files
      - filebrowser-uploads:/.uploads
    command: ["--enable-upload", "--address", "0.0.0.0"]
    environment:
      TITLE: File Server
      EXTRA_HEADERS: ""
    restart: unless-stopped

volumes:
  filebrowser-uploads:
```

Start it and open http://localhost:7667:

```sh
docker compose up -d
```
