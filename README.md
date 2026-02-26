# Outbound

Outbound is a minimal HTTP tunneling system that exposes local services through a public edge server using a long-lived gRPC stream.

## How it works

```
[HTTP client] ── HTTPS ──▶ [edge :443 via proxy] ── gRPC ──▶ [agent] ── HTTP ──▶ [local service]
```

1. The **edge** runs on a public server. It accepts HTTP requests and dispatches them over gRPC to the appropriate agent.
2. The **agent** runs on your local machine. It connects to the edge, registers named services, and proxies inbound requests to local ports.
3. Routing is header-based — callers include `X-Outbound-Agent` and `X-Outbound-Service` on every request.

## Quickstart

### 1. Build

```bash
go build -o outbound-edge ./cmd/edge
go build -o outbound-agent ./cmd/agent
```

### 2. Run the edge (on your public server)

```bash
./outbound-edge --http-addr :8080 --grpc-addr :8081
```

### 3. Run the agent (on your local machine)

Expose a local service running on port 3000 as the service named `web`:

```bash
./outbound-agent \
  --id my-machine \
  --service web=3000 \
  --edge edge.example.com:443
```

Pass `--insecure` for local development when the edge has no TLS:

```bash
./outbound-agent --id my-machine --service web=3000 --edge localhost:8081 --insecure
```

### 4. Make a request

```bash
curl \
  -H "X-Outbound-Agent: my-machine" \
  -H "X-Outbound-Service: web" \
  https://edge.example.com/
```

### 5. Multiple services

```bash
./outbound-agent \
  --id my-machine \
  --service web=3000 \
  --service api=8000 \
  --service metrics=9090 \
  --edge edge.example.com:443
```

```bash
curl -H "X-Outbound-Agent: my-machine" -H "X-Outbound-Service: api"     https://edge.example.com/v1/users
curl -H "X-Outbound-Agent: my-machine" -H "X-Outbound-Service: metrics" https://edge.example.com/metrics
```

## HTTPS and custom domains

TLS is not handled inside the edge binary. Put Caddy (or any reverse proxy) in front of the edge and terminate TLS there. The proxy forwards HTTP traffic to `:8080` and gRPC traffic to `:8081`.

```
[HTTP client]  ─── HTTPS (443)    ───┐
                                     [Caddy] ─── plain HTTP  ──▶ edge :8080
[agent]        ─── gRPC+TLS (443) ───┘        ─── plain gRPC ──▶ edge :8081
```

### Caddy

```caddy
edge.example.com {
    @grpc {
        header Content-Type application/grpc*
    }
    reverse_proxy @grpc h2c://127.0.0.1:8081
    reverse_proxy 127.0.0.1:8080
}
```

Keep both ports bound to `127.0.0.1` so only the proxy can reach them.

## Security

Outbound is a v1 proof-of-concept. Known gaps:

- **No agent authentication.** Any gRPC client that can reach `:8081` can register as any agent ID and receive its traffic. Keep `:8081` bound to `127.0.0.1`.
- **No HTTP caller authentication.** Any client that knows a valid agent ID and service name can route requests through the edge. Add a bearer token check at the reverse proxy layer if needed.
- **No rate limiting.** A malicious edge can flood an agent with requests; a malicious agent can exhaust edge goroutines.
- **Response body size is unbounded.** Large backend responses are buffered fully in memory on both the agent and the edge.
- **Single-value headers only.** Multi-value headers (e.g. multiple `Set-Cookie`) are collapsed to a comma-joined string.

## Flags reference

### edge

| Flag | Default | Description |
|------|---------|-------------|
| `--http-addr` | `:8080` | Address for the public HTTP listener |
| `--grpc-addr` | `:8081` | Address for the agent gRPC listener |

### agent

| Flag | Default | Description |
|------|---------|-------------|
| `--id` | _(auto-generated)_ | Agent identifier |
| `--edge` | `localhost:8081` | Edge address (`host:port`) |
| `--service` | _(required, repeatable)_ | Service mapping `name=port` |
| `--insecure` | `false` | Disable TLS on the gRPC connection (local dev only) |
