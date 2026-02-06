# minitunnel

A lightweight ngrok-like tunnel service in Go. Exposes local services through a public server using WebSocket-based tunneling with path-based routing.

## How it works

```
Internet → Server (:8080) → WebSocket → Client → Local Service (:3000)
```

1. The **server** accepts HTTP requests and maintains WebSocket connections to tunnel clients
2. The **client** connects to the server, registers a service name, and forwards requests to a local target
3. Requests to `server:8080/myservice/api/hello` get forwarded to `localhost:3000/api/hello` (prefix stripped)

## Build

```bash
go build -o minitunnel-server ./cmd/server
go build -o minitunnel-client ./cmd/client
```

## Usage

### Server

```bash
export AUTH_TOKEN=my-secret-token
export PORT=8080  # optional, defaults to 8080
./minitunnel-server
```

### Client

```bash
export SERVER_URL=http://your-server:8080
export AUTH_TOKEN=my-secret-token
export SERVICE_NAME=myapp
export LOCAL_TARGET=localhost:3000
./minitunnel-client
```

Now requests to `http://your-server:8080/myapp/any/path` are forwarded to `http://localhost:3000/any/path`.

### Docker

```bash
docker compose up
```

Or build and run manually:

```bash
docker build -t minitunnel-server .
docker run -p 8080:8080 -e AUTH_TOKEN=my-secret-token minitunnel-server
```

## Environment Variables

### Server

| Variable     | Required | Default | Description          |
|-------------|----------|---------|----------------------|
| `AUTH_TOKEN` | Yes      | —       | Token for client auth |
| `PORT`       | No       | `8080`  | HTTP listen port      |

### Client

| Variable       | Required | Description                              |
|---------------|----------|------------------------------------------|
| `SERVER_URL`   | Yes      | Server URL (e.g., `http://server:8080`)  |
| `AUTH_TOKEN`   | Yes      | Auth token (must match server)           |
| `SERVICE_NAME` | Yes      | Service name for path routing            |
| `LOCAL_TARGET` | Yes      | Local address to forward to (e.g., `localhost:3000`) |

## Protocol

JSON messages over WebSocket:

**Server → Client (request):**
```json
{"id": "uuid", "method": "GET", "path": "/api/hello", "headers": {}, "body": ""}
```

**Client → Server (response):**
```json
{"id": "uuid", "status": 200, "headers": {}, "body": "response body"}
```

## Features

- Path-based routing with prefix stripping
- Token-based authentication
- Request multiplexing over a single WebSocket per client
- Auto-reconnect with exponential backoff (client)
- Clean shutdown on SIGINT/SIGTERM (client)
- Multi-stage Docker build
