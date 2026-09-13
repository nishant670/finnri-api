#!/bin/sh
set -e

# Why this file exists.
#
# The image creates /app/uploads and hands it to the unprivileged `finnri` user
# at build time. Mounting a volume on that path at runtime replaces the
# directory wholesale — the build-time `chown` goes with it, and what the
# container actually sees is a fresh mount owned by root with mode 0755. A
# process already running as `finnri` cannot write into it and cannot chown it
# either, so every receipt upload failed with a 500 the moment a volume was
# attached. The directory looked right in the Dockerfile and was wrong on disk.
#
# So the container starts as root, takes ownership of whatever is actually
# mounted, and only then drops to `finnri` for the life of the process. The
# server itself never runs with privileges.
UPLOAD_DIR=/app/uploads

mkdir -p "$UPLOAD_DIR"
# Ownership only, and only on the upload directory. `-R` on a volume holding
# thousands of receipts would add a full tree walk to every cold start.
chown finnri:finnri "$UPLOAD_DIR" 2>/dev/null || true

# Already unprivileged (a platform that pins the user, or a local `docker run
# --user`): there is nothing to drop, so run in place rather than failing.
if [ "$(id -u)" != "0" ]; then
    exec "$@"
fi

exec su-exec finnri:finnri "$@"
