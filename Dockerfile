FROM golang:1.27-alpine AS builder
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kitchen-api ./cmd/kitchen-api \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kitchen-worker ./cmd/kitchen-worker \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/restaurant-demo ./cmd/restaurant-demo \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/goose github.com/pressly/goose/v3/cmd/goose

FROM alpine:3.23
RUN apk add --no-cache ca-certificates curl && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /out/* /usr/local/bin/
COPY migrations ./migrations
USER app
ENTRYPOINT []
CMD ["kitchen-api"]
