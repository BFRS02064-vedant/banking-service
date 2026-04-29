# Stage 1: build the server binary using the vendored module cache.
FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -o /out/server ./cmd/server

# Stage 2: minimal runtime image.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /out/server /app/server
COPY migrations /app/migrations
COPY web /app/web

EXPOSE 8080

ENTRYPOINT ["/app/server"]
