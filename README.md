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

# Browser upload resuming

Browser uploads start immediately without hashing the file. A random 128-bit
upload ID is saved in localStorage using the destination, file name, size,
modification time, and temporary-upload setting. Reselect the same file after a
reload to resume an interrupted upload. Resume state expires on the server after
24 hours; completing or canceling an upload clears its local ID. If browser
storage is unavailable, retries still work within the current page.

File metadata is used to recognize a reselected file; it does not verify file
contents. API clients can still opt into content verification with Upload-SHA256.
