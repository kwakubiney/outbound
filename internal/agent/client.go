package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"outbound/internal/tunnel"
	tunnelpb "outbound/proto"
)

type Client struct {
	agentID  string
	services map[string]uint16
	stream   tunnelpb.TunnelService_ConnectClient
	client   *http.Client
	sendMu   sync.Mutex
}

func NewClient(agentID string, services map[string]uint16) *Client {
	return &Client{
		agentID:  agentID,
		services: services,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Run(ctx context.Context, stream tunnelpb.TunnelService_ConnectClient) error {
	c.stream = stream
	if err := c.register(); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Msg.(type) {
		case *tunnelpb.TunnelMessage_ReqStart:
			go c.handleRequest(ctx, payload.ReqStart)
		case *tunnelpb.TunnelMessage_Ping:
			pong := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Pong{Pong: &tunnelpb.Pong{TsUnixMs: payload.Ping.TsUnixMs}}}
			_ = c.send(pong)
		}
	}
}

func (c *Client) register() error {
	services := make([]*tunnelpb.Service, 0, len(c.services))
	for name, port := range c.services {
		services = append(services, &tunnelpb.Service{Name: name, Port: uint32(port)})
	}
	msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Register{Register: &tunnelpb.RegisterRequest{
		AgentId:  c.agentID,
		Services: services,
	}}}
	return c.send(msg)
}

func (c *Client) handleRequest(ctx context.Context, req *tunnelpb.RequestStart) {
	port, ok := c.services[req.Service]
	if !ok {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 404, Message: "service not found"}}})
		return
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, req.Path)
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, bytes.NewReader(req.Body))
	if err != nil {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 500, Message: "failed to create request"}}})
		return
	}

	if host, ok := req.Headers["Host"]; ok {
		httpReq.Host = host
		delete(req.Headers, "Host")
	}

	tunnel.MapToHeaders(httpReq.Header, req.Headers)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 502, Message: "local request failed"}}})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 502, Message: "failed to read response"}}})
		return
	}

	headers := tunnel.HeadersToMap(resp.Header, nil)
	msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ResStart{ResStart: &tunnelpb.ResponseStart{
		RequestId: req.RequestId,
		Status:    int32(resp.StatusCode),
		Headers:   headers,
		Body:      body,
	}}}
	_ = c.send(msg)
}

func (c *Client) send(msg *tunnelpb.TunnelMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(msg)
}

func NormalizeServiceMap(entries map[string]uint16) map[string]uint16 {
	clean := make(map[string]uint16, len(entries))
	for name, port := range entries {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		clean[trimmed] = port
	}
	return clean
}
