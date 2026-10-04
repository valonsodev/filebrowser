# installation

```yaml
services:
  bin:
    image: ghcr.io/francorbacho/filebrowser
    ports:
      - 7667:8000
    volumes:
      - /data/bin:/files
    command: --enable-upload --address 0.0.0.0
    environment:
      - TITLE="File Server"
      - EXTRA_HEADERS=""
    cap_add:
      - NET_BIND_SERVICE
    restart: unless-stopped
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

With `--enable-upload` or `ENABLE_UPLOAD=true`, **Delete** removes a file after confirmation.

Turn on **Temporary uploads (5 min)** before uploading to have new uploads deleted automatically five minutes after each upload completes. It applies to file selection, pasted text, and drag-and-drop uploads. Turning it off makes subsequent uploads permanent; queued or in-progress uploads keep the setting they started with. File rows show a countdown beside Delete without wrapping actions onto another line.

Deletion runs on the server even when the browser is closed. Expiry times are stored in the reserved `.filebrowser` directory on the files volume and survive restarts; overdue files are removed at startup. Uploading a permanent replacement clears its previous expiry.

API clients can append `?temporary=true` to a PUT or resumable upload creation request to upload a temporary file. The completion response includes `File-Expires-At` as Unix seconds. They can use `DELETE /path/to/file` to delete a file, `PATCH /path/to/file?temporary=true` to schedule expiry, or `PATCH /path/to/file?temporary=false` to cancel it. File mutations share the upload setting and its existing access model.

Use the **Folder name** row and **Create folder** button to create a folder in the current directory. Enter submits the name; errors appear below the row. The new folder appears in the list without navigating away. Folder creation requires uploads to be enabled. API clients can use `POST /path/to/new-folder/?folder=true`; the parent directory must exist, and existing files or folders are never overwritten.

# downloads

File responses include a `Content-Disposition` filename. Use `curl -OJ` to save a download using the filename supplied by the server, including spaces and Unicode characters:

```sh
curl -OJ 'http://localhost:8000/my%20file.txt'
```

Files are served as attachments with `X-Content-Type-Options: nosniff` so uploaded HTML downloads instead of opening as a page on the browser's origin. Browser requests that change files must come from the same origin; command-line clients can continue to use the API without browser headers. Symlinks in the files tree are not accessible through file URLs, including aliases to the private expiry directory. Uploads are staged and installed atomically, so interrupted transfers leave the existing file intact.
