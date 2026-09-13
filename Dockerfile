# Build a static Go binary; the server needs no CGO because the sqlite driver
# is only used by tests.
FROM golang:1.24-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM alpine:3.20

# ca-certificates for outbound HTTPS (OpenAI, Google token verification, Postgres TLS).
# tzdata because handlers call time.LoadLocation("Asia/Kolkata").
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=build /out/server /app/server
COPY --from=build /out/migrate /app/migrate
COPY schemas /app/schemas
COPY migrations /app/migrations

# su-exec drops privileges from the entrypoint; see entrypoint.sh for why the
# container cannot simply start as an unprivileged user.
RUN apk add --no-cache su-exec

# Receipt uploads are written here. Mount a Railway volume on this path so the
# files survive redeploys.
RUN addgroup -S finnri \
    && adduser -S -G finnri finnri \
    && mkdir -p /app/uploads \
    && chown -R finnri:finnri /app/uploads

COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Deliberately no `USER finnri` here. A mounted volume arrives owned by root and
# shadows the image's own `/app/uploads` along with the `chown` above, so a
# container that has already dropped privileges can never take ownership of it —
# which is exactly how receipt uploads came to fail with "failed to save file"
# on every deploy that had the volume attached. The entrypoint starts as root,
# fixes the mount, and then drops to `finnri` before exec'ing the server.
EXPOSE 8080

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["/app/server"]
