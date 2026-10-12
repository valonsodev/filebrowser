// Combined with the pinned js-sha256 library by the server. No network imports.
self.onmessage = async function(event) {
  try {
    const file = event.data;
    const hash = sha256.create();
    const chunkSize = 5 * 1024 * 1024;
    for (let offset = 0; offset < file.size; offset += chunkSize) {
      const end = Math.min(offset + chunkSize, file.size);
      hash.update(await file.slice(offset, end).arrayBuffer());
      self.postMessage({ loaded: end });
    }
    self.postMessage({ hash: hash.hex() });
  } catch (error) {
    self.postMessage({ error: error.message || 'Unable to read file' });
  }
};
