#!/usr/bin/env bash
# ============================================================
# demo_body_limit.sh
#
# Demonstrates expected behavior after chunked streaming fix.
# Bodies are streamed in small chunks over gRPC instead of being
# buffered as a single protobuf message.
#
# Expected results with MaxRequestBody=10MB:
#   - 1KB body  -> HTTP 200
#   - 5MB body  -> HTTP 200 (chunked tunnel transfer)
#   - 11MB body -> HTTP 413 (edge max body guardrail)
#
# Usage: bash demo_body_limit.sh
# ============================================================

EDGE_HTTP=":8080"
EDGE_GRPC=":8081"
DEMO_PORT="3001"

EDGE_PID=""
AGENT_PID=""
DEMO_PID=""

cleanup() {
    echo ""
    echo "--- cleaning up ---"
    kill "$EDGE_PID" "$AGENT_PID" "$DEMO_PID" 2>/dev/null || true
    rm -f /tmp/outbound_payload_*.bin /tmp/outbound_resp_*.txt
}
trap cleanup EXIT

start_agent() {
    /tmp/outbound-agent \
        -id demo-agent \
        -edge "localhost:8081" \
        -insecure \
        -service "demo=$DEMO_PORT" \
        >> /tmp/agent.log 2>&1 &
    AGENT_PID=$!
    sleep 1
}

wait_for_tunnel_ready() {
    for i in $(seq 1 60); do
        CODE=$(curl -s -o /dev/null -w "%{http_code}" \
            -X POST http://localhost:8080/ \
            -H "x-outbound-agent: demo-agent" \
            -H "x-outbound-service: demo" \
            --data-binary "") || true
        if [ "$CODE" = "200" ]; then
            return 0
        fi
        sleep 0.25
    done
    return 1
}

echo "======================================================"
echo "  Outbound large-body demo"
echo "======================================================"
echo ""

# Kill anything already on these ports
echo "[0/5] freeing ports 8080, 8081, 3001..."
lsof -ti:8080,8081,3001 | xargs kill -9 2>/dev/null || true
sleep 0.3
echo "      done."
echo ""

# Build binaries
echo "[1/5] building binaries..."
go build -o /tmp/outbound-edge  ./cmd/edge  || { echo "FATAL: edge build failed"; exit 1; }
go build -o /tmp/outbound-agent ./cmd/agent || { echo "FATAL: agent build failed"; exit 1; }
go build -o /tmp/outbound-demo  ./cmd/demo  || { echo "FATAL: demo build failed"; exit 1; }
echo "      done."
echo ""

# Start edge
echo "[2/5] starting edge (HTTP $EDGE_HTTP, gRPC $EDGE_GRPC)..."
/tmp/outbound-edge \
    -http-addr "$EDGE_HTTP" \
    -grpc-addr "$EDGE_GRPC" \
    -max-request-body $((10 * 1024 * 1024)) \
    > /tmp/edge.log 2>&1 &
EDGE_PID=$!
echo "      edge PID $EDGE_PID — waiting for HTTP port..."
for i in $(seq 1 40); do
    curl -s -o /dev/null http://localhost:8080/ && break || true
    sleep 0.25
done
echo "      edge ready."
echo ""

# Start demo backend
echo "[3/5] starting demo backend on :$DEMO_PORT..."
PORT=$DEMO_PORT /tmp/outbound-demo > /tmp/demo.log 2>&1 &
DEMO_PID=$!
echo "      demo PID $DEMO_PID — waiting for demo port..."
for i in $(seq 1 20); do
    curl -s -o /dev/null "http://localhost:$DEMO_PORT/" && break || true
    sleep 0.25
done
echo "      demo ready."
echo ""

# Start agent
echo "[4/5] starting agent (service demo=$DEMO_PORT)..."
start_agent
echo "      agent PID $AGENT_PID — waiting for agent to register..."
if wait_for_tunnel_ready; then
    echo "      agent ready."
else
    echo "FATAL: tunnel never became ready (see /tmp/edge.log and /tmp/agent.log)"
    exit 1
fi
echo ""

echo "[5/5] running tests..."
echo ""

# -----------------------------------------------------------------
# TEST A: small body — should work fine
# -----------------------------------------------------------------
echo "--- TEST A: 1KB body (should succeed) ---"
dd if=/dev/urandom of=/tmp/outbound_payload_1k.bin bs=1024 count=1 2>/dev/null
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST http://localhost:8080/ \
    -H "x-outbound-agent: demo-agent" \
    -H "x-outbound-service: demo" \
    --data-binary @/tmp/outbound_payload_1k.bin) || true
echo "    HTTP status: $HTTP_CODE"
if [ "$HTTP_CODE" = "200" ]; then
    echo "    PASS: small body tunnelled successfully"
else
    echo "    UNEXPECTED: got $HTTP_CODE (check /tmp/edge.log, /tmp/agent.log)"
fi
echo ""

# -----------------------------------------------------------------
# -----------------------------------------------------------------
# TEST B: 5MB body — should succeed after chunked streaming fix
# -----------------------------------------------------------------
echo "--- TEST B: 5MB body (should succeed with streaming) ---"
echo "    Generating 5MB payload..."
dd if=/dev/urandom of=/tmp/outbound_payload_5m.bin bs=$((1024*1024)) count=5 2>/dev/null
echo "    Sending to edge..."
HTTP_CODE=$(curl -s -o /tmp/outbound_resp_5m.txt -w "%{http_code}" \
    -X POST http://localhost:8080/ \
    -H "x-outbound-agent: demo-agent" \
    -H "x-outbound-service: demo" \
    --data-binary @/tmp/outbound_payload_5m.bin) || true
echo "    HTTP status: $HTTP_CODE"
if [ "$HTTP_CODE" = "200" ]; then
    echo "    PASS: 5MB body tunnelled successfully via chunks"
else
    echo "    UNEXPECTED: got $HTTP_CODE (check /tmp/edge.log, /tmp/agent.log)"
fi
echo ""

# -----------------------------------------------------------------
# TEST C: 11MB body — exceeds edge MaxRequestBody guardrail (10MB)
# -----------------------------------------------------------------
echo "--- TEST C: 11MB body (expect 413 -- edge max-request-body) ---"
echo "    Generating 11MB payload..."
dd if=/dev/urandom of=/tmp/outbound_payload_11m.bin bs=$((1024*1024)) count=11 2>/dev/null
echo "    Sending to edge..."
HTTP_CODE=$(curl -s -o /tmp/outbound_resp_11m.txt -w "%{http_code}" \
    -X POST http://localhost:8080/ \
    -H "x-outbound-agent: demo-agent" \
    -H "x-outbound-service: demo" \
    --data-binary @/tmp/outbound_payload_11m.bin) || true
echo "    HTTP status: $HTTP_CODE"
BODY=$(cat /tmp/outbound_resp_11m.txt 2>/dev/null || true)
echo "    Response body: $BODY"
if [ "$HTTP_CODE" = "413" ]; then
    echo "    PASS: edge guardrail enforced at 10MB"
else
    echo "    UNEXPECTED: got $HTTP_CODE"
fi
echo ""

# -----------------------------------------------------------------
# Summary
# -----------------------------------------------------------------
echo "======================================================"
echo "  Result"
echo "======================================================"
echo ""
echo "  1KB request  -> expected 200"
echo "  5MB request  -> expected 200 (streamed over gRPC in chunks)"
echo "  11MB request -> expected 413 (edge MaxRequestBody=10MB)"
echo ""
echo "  This verifies that chunked tunneling works for multi-MB bodies"
echo "  while keeping the 10MB edge guardrail in place."
echo ""
