# installation

```yaml
services:
  bin:
    image: ghcr.io/francorbacho/filebrowser
    ports:
      - 7667:8000
    volumes:
      - /data/bin:/files
      - filebrowser-uploads:/.uploads
    command: --enable-upload --address 0.0.0.0
    environment:
      - TITLE="File Server"
      - EXTRA_HEADERS=""
    cap_add:
      - NET_BIND_SERVICE
    restart: unless-stopped

volumes:
  filebrowser-uploads:
```

# images

![filebrowser's dark theme](https://files.fran.cam/static/filebrowser-dark.png)

![filebrowser's light theme](https://files.fran.cam/static/filebrowser-light.png)

# metrics

- `filebrowser_info` - Build info
- `filebrowser_uptime_seconds` - Uptime
- `filebrowser_http_requests_total{status}` - HTTP requests
- `filebrowser_uploads_total{status}` - Uploads
- `filebrowser_operations_total{type}` - File operations
- `filebrowser_memory_bytes{type}` - Memory usage
- `filebrowser_goroutines` - Goroutines
- `filebrowser_gc_total` - GC count
- `filebrowser_config{setting}` - Config

# file actions

With `--enable-upload` or `ENABLE_UPLOAD=true`, **Delete** removes a file immediately.

Turn on **Temporary uploads (5 min)** before uploading to have new uploads deleted automatically five minutes after each upload completes. It applies to file selection, pasted text, and drag-and-drop uploads. Turning it off makes subsequent uploads permanent; queued or in-progress uploads keep the setting they started with. File rows show a countdown beside Delete without wrapping actions onto another line.

Deletion runs on the server even when the browser is closed. Expiry times are stored in the reserved `.filebrowser` directory on the files volume and survive restarts; overdue files are removed at startup. Uploading a permanent replacement clears its previous expiry.

API clients can append `?temporary=true` to a PUT or resumable upload creation request to upload a temporary file. The completion response includes `File-Expires-At` as Unix seconds. They can use `DELETE /path/to/file` to delete a file, `PATCH /path/to/file?temporary=true` to schedule expiry, or `PATCH /path/to/file?temporary=false` to cancel it. File mutations share the upload setting and its existing access model.

File uploads use resumable transfers, including temporary uploads. Before sending bytes, the browser calculates the file's SHA-256 in a worker using small chunks, then asks the server for a matching unfinished upload. After closing the window, clearing browser storage, or restarting the server, select the same file in the same folder with the same temporary setting and start uploading again. The server returns the existing upload URL and byte offset; file timestamps and browser storage do not affect discovery. The browser needs you to select the file again because it cannot keep access to the original file across window closures.

Partial files and upload metadata are stored in `/.uploads` in the container (`./.uploads` when running the binary directly). Mount this directory on persistent storage to survive container replacements; the Compose example above uses a named volume. Unfinished uploads are discoverable for 24 hours of inactivity, and abandoned resources are cleaned up periodically. Partials created by older versions without metadata cannot be recovered. On completion, the server verifies the full SHA-256 before replacing the destination. Temporary files expire five minutes after completion, regardless of how long uploading takes.

API clients can discover or create an unfinished upload with an empty POST to the destination, using these headers:

```http
Upload-Complete: ?0
Upload-Length: <file size in bytes>
Upload-SHA256: <64 lowercase hex characters>
```

Use `?temporary=true` for temporary uploads. This application's optional `Upload-SHA256` extension matches the hash, destination, size, and temporary setting. The response is `201 Created` for a new resource or `200 OK` for a matching unfinished one, with `Location`, `Upload-Offset`, and `Upload-SHA256` headers. Continue with PATCH requests to that location, using the returned offset and `Content-Type: application/partial-upload`; mark the last PATCH `Upload-Complete: ?1`. A checksum mismatch returns `422` and discards the invalid partial, preserving any existing destination file. Requests without a hash continue to use the original resumable API.

Use the **Folder name** row and **Create folder** button to create a folder in the current directory. Enter submits the name; errors appear below the row. The new folder appears in the list without navigating away. Folder creation requires uploads to be enabled. API clients can use `POST /path/to/new-folder/?folder=true`; the parent directory must exist, and existing files or folders are never overwritten.

# downloads

Directory pages return a Markdown file listing when the request prefers `text/markdown` in its `Accept` header or uses a curl user agent. The listing contains file and folder links, sizes, modification dates, and expiry times, with absolute URLs and UTC timestamps. Other requests receive the HTML page. Both representations advertise the Markdown alternative and vary on `Accept` and `User-Agent` for caching. Individual file URLs always serve the original download.

File responses include a `Content-Disposition` filename. Use `curl -OJ` to save a download using the filename supplied by the server, including spaces and Unicode characters:

```sh
curl -OJ 'http://localhost:8000/my%20file.txt'
```

Files are served as attachments with `X-Content-Type-Options: nosniff` so uploaded HTML downloads instead of opening as a page on the browser's origin. Browser requests that change files must come from the same origin; command-line clients can continue to use the API without browser headers. Symlinks in the files tree are not accessible through file URLs, including aliases to the private expiry directory. Uploads are staged and installed atomically, so interrupted transfers leave the existing file intact.
