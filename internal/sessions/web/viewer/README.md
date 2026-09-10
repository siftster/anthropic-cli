`viewer.html.gz` is the single-file browser viewer served by
`ant beta:sessions connect --web`: a prebuilt build of the Console session
viewer, vendored here as an opaque artifact (its source is not part of this
repository), stored gzipped and embedded into the binary as-is. What it
expects from this process — the injected config, the SSE frames, the send
bodies — is the wire protocol implemented in `../server.go` (config, send
bodies) and `../hub.go` (SSE frames).

To see which build is vendored, read the `session-viewer-build` meta tag:

    gunzip -c viewer.html.gz | grep -o '<meta name="session-viewer-build"[^>]*>'

To update, gzip a newer build over the file (`gzip -9 -n` keeps the output
reproducible):

    gzip -9 -n -c path/to/viewer.html > viewer.html.gz
