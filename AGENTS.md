# Outbound

Outbound is a reverse tunnel written in Go. Agents running behind firewalls
open a persistent bidirectional gRPC stream to the edge server. The edge
forwards inbound HTTP requests through that tunnel and streams the responses
back to callers.

## Project structure

```
cmd/agent/      Agent binary entrypoint (--token, --service, --edge, --insecure)
cmd/edge/       Edge binary entrypoint (--auth-secret, --http-addr, --grpc-addr)
internal/agent/ Agent client: register, proxy HTTP upstream, reconnect loop
internal/edge/  Edge server: session management, auth, HTTP dispatch
internal/tunnel Shared constants (headers, request ID generation, header helpers)
proto/          Protobuf definitions (TunnelService, RegisterRequest, etc.)
internal/tunneltest/ In-process test harness using bufconn and httptest.Server
```

## Architecture decisions

**Auth.** Single-tenant pre-shared secret. The edge validates the token on registration. On rejection, the connection is closed cleanly and the agent does not retry.

**Reconnect.** The agent reconnect loop uses exponential backoff with jitter. The connection pool is reused across reconnects.

**Keepalive.** The edge sends Ping frames on a configurable interval and expects a Pong. A missing Pong drops the session.

**Session lifecycle.** Register attaches a session keyed by agentID. A duplicate registration on an established session is ignored.

**Dispatch.** The edge receive goroutine is isolated so keepalive and dispatch never block on a slow stream. Response dispatch is non-blocking to prevent buffer overflow.

**Buffers.** Read buffers are pooled in both the agent and the edge to reduce GC pressure.

## Conventions

Commit messages use `type: subject` on the first line followed by a blank line
and a prose body written in full sentences. Do not use bullet points or dashes
in commit messages.

Keep changes minimal and focused. Do not introduce new dependencies without
discussion. Do not widen test assertions to accept bad product behaviour; flag
the unexpected behaviour instead.

The test harness runs the edge and agent in-process. Timing-based assertions are not acceptable; use a deterministic polling loop with a fixed deadline instead.

Only comment code that involves non-obvious concurrency decisions. Remove
explanatory comments that restate what the code already clearly says.
