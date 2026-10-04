# ── build ────────────────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/adesgo ./cmd/adesgo \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/adesgo-numeri ./cmd/numeri

# ── run ──────────────────────────────────────────────────────────────────────
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Europe/Rome \
    DB_PATH=/app/data/wine.db \
    HTTP_ADDR=:8080
WORKDIR /app
COPY --from=build /out/adesgo /out/adesgo-numeri /usr/local/bin/
VOLUME /app/data
EXPOSE 8080
ENTRYPOINT ["adesgo"]
