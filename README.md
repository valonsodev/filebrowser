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
modification time, and temporary/protected-upload settings. Reselect the same file after a
reload to resume an interrupted upload. Resume state expires on the server after
24 hours; completing or canceling an upload clears its local ID. If browser
storage is unavailable, retries still work within the current page.

File metadata is used to recognize a reselected file; it does not verify file
contents. API clients can still opt into content verification with Upload-SHA256.

# Password protected files

Enable the lock toggle next to the 5-minute toggle and enter a password. Files
and pasted text queued while the lock is enabled use that password; turning it
off clears the selected password without changing uploads already queued. Both
toggles can be enabled together. After a reload, enable the lock and enter the
same password before reselecting a protected upload to resume it.

Protected files appear as anonymous locked entries with opaque links. HTML and
Markdown listings hide their original names, sizes and modification times;
directory sizes exclude them. Their presence, containing directory and any
expiry countdown remain visible. Clicking an entry asks for its password and
downloads it with its original filename. Opening a shared opaque link also shows
a password form. The Copy button includes a placeholder header:

```sh
curl -fLOJ -H 'File-Password: YOUR_PASSWORD' -- 'https://example.com/.protected/OPAQUE_ID'
```

API clients supply the same `File-Password` header to create a protected upload,
download it, overwrite or delete it, change its expiry, and access its resumable
upload resource (including HEAD, PATCH and DELETE). Overwriting a protected file
requires its existing password and retains protection. Passwords are limited to
1–1024 printable ASCII characters; empty or duplicate headers are rejected.
Passwords in query strings are not supported.

Only independently salted Argon2id verifiers are persisted, in private server
metadata. Browser passwords stay in memory and are never saved in localStorage
or copied into curl commands. Verification has a server-wide rate limit and
bounded worker concurrency, and responses use `Cache-Control: no-store`. Protect
both the `files` volume (including `.filebrowser`) and `.uploads` when backing up
or moving data: losing password metadata loses access control. File contents and
original names on disk are not encrypted. Serve over HTTPS outside localhost so
credentials and content are protected in transit; configure proxies to avoid
logging the `File-Password` header. Browser downloads buffer the file in memory;
use curl for very large protected files.
