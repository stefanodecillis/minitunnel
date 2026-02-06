FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /minitunnel-server ./cmd/server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /minitunnel-server /usr/local/bin/minitunnel-server

EXPOSE 8080
ENTRYPOINT ["minitunnel-server"]
