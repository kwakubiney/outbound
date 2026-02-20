# Outbound

Outbound is a minimal HTTP tunneling system that exposes local services through a public edge using a long‑lived gRPC stream.

## How it works

- The edge listens for public HTTP requests and forwards them over gRPC.
- The agent connects to the edge, registers services, and proxies to localhost.
- Routing is header‑based.

Required headers:

- `X-Outbound-Agent`
- `X-Outbound-Service`

## Quickstart

Build:

```bash
go build -o outbound-edge ./cmd/edge
go build -o outbound-agent ./cmd/agent
```

Run edge (public server):

```bash
./outbound-edge --http-addr :8080 --grpc-addr :8081
```

Run agent (local machine):

```bash
./outbound-agent --id laptop-1 --service api=2000 --edge <EDGE_HOST>:8081
```

Test:

```bash
curl \
  -H "X-Outbound-Agent: laptop-1" \
  -H "X-Outbound-Service: api" \
  http://<EDGE_HOST>:8080/
```
