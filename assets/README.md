Browser hashing uses js-sha256 v0.11.1, vendored from
https://github.com/emn178/js-sha256/tree/v0.11.1 under the accompanying MIT license.
The server embeds it with upload-hash-worker.js into the directory page. A worker
hashes files in 5 MiB chunks, including on HTTP origins, without external requests
or keeping the entire file in memory.
