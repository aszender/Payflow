FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/payflow ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && addgroup -S payflow && adduser -S -G payflow payflow
WORKDIR /app
COPY --from=builder /out/payflow /app/payflow
COPY migrations /app/migrations
EXPOSE 8080
USER payflow
ENTRYPOINT ["/app/payflow"]
