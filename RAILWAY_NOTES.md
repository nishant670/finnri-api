# Railway runtime notes

- Keep `deploy.numReplicas` at `1` while HTTP rate limits use the in-process
  token buckets in `internal/http/middleware.go`. Every additional replica has
  an independent bucket and therefore multiplies the effective limit.
- Before increasing the replica count, move rate-limit state to a shared store
  such as Redis and test that all instances observe the same counters.
- Mount a persistent Railway volume at `/app/uploads`; receipt files stored on
  the container filesystem are otherwise lost on redeploy.
- That mount arrives owned by `root` and shadows the image's own `/app/uploads`
  along with its build-time `chown`, so the container starts as root, takes
  ownership of the mount in `entrypoint.sh`, and drops to the unprivileged
  `finnri` user before running the server. Do not add `USER finnri` back to the
  Dockerfile: the server would then be unable to write to the volume, and every
  receipt upload fails with a 500 and "Failed to save file" in the app.
- `upload_storage_ready path=…` in the deploy log means receipts can be saved.
  `upload_storage_not_writable` or `upload_storage_unusable` means they cannot,
  and names the path to check.
