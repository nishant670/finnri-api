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

# Receipt uploads are written here. Mount a Railway volume on this path so the
# files survive redeploys.
RUN addgroup -S finnri \
    && adduser -S -G finnri finnri \
    && mkdir -p /app/uploads \
    && chown -R finnri:finnri /app/uploads

USER finnri

EXPOSE 8080

CMD ["/app/server"]
