FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -o /payflow ./cmd/server

FROM alpine:3.18
COPY --from=builder /payflow /payflow
EXPOSE 8080
ENTRYPOINT ["/payflow"]
