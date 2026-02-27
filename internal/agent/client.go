package agent

import (
	"context"
	"errors"
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
	inFlight sync.Map
}

type inFlightRequest struct {
	bodyWriter *io.PipeWriter
}

const streamChunkSize = 64 * 1024

func NewClient(agentID string, services map[string]uint16) *Client {
	return &Client{
		agentID:  agentID,
		services: services,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Run(ctx context.Context, stream tunnelpb.TunnelService_ConnectClient) error {
	c.stream = stream
	//registers the services that this agent wants to expose
	if err := c.register(); err != nil {
		return err
	}

	//wait for the edge to acknowledge the registration before processing any requests. This ensures the agent doesn't miss any early incoming requests that arrive before the ack.
	if err := c.awaitRegisterAck(); err != nil {
		return err
	}

	//start looping to recv messages from the edge because we need to receive from e.
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Msg.(type) {
		case *tunnelpb.TunnelMessage_ReqStart:
			c.startRequest(ctx, payload.ReqStart)
		case *tunnelpb.TunnelMessage_ReqBody:
			c.handleBodyChunk(payload.ReqBody)
		case *tunnelpb.TunnelMessage_Error:
			c.handleRemoteError(payload.Error)
		case *tunnelpb.TunnelMessage_Ping:
			pong := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Pong{Pong: &tunnelpb.Pong{TsUnixMs: payload.Ping.TsUnixMs}}}
			_ = c.send(pong)
		}
	}
}

func (c *Client) awaitRegisterAck() error {
	for {
		msg, err := c.stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Msg.(type) {
		case *tunnelpb.TunnelMessage_RegisterAck:
			if payload.RegisterAck.GetOk() {
				return nil
			}
			message := strings.TrimSpace(payload.RegisterAck.GetMessage())
			if message == "" {
				message = "registration rejected"
			}
			return errors.New(message)
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

func (c *Client) startRequest(ctx context.Context, req *tunnelpb.RequestStart) {
	port, ok := c.services[req.Service]
	if !ok {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 404, Message: "service not found"}}})
		return
	}

	bodyReader, bodyWriter := io.Pipe()
	// Register in-flight state before spinning the worker goroutine so
	// early req_body frames can be routed immediately without being dropped.
	c.inFlight.Store(req.RequestId, &inFlightRequest{bodyWriter: bodyWriter})
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, req.Path)
	go c.forwardRequest(ctx, req, url, bodyReader, bodyWriter)
}

func (c *Client) forwardRequest(ctx context.Context, req *tunnelpb.RequestStart, url string, bodyReader *io.PipeReader, bodyWriter *io.PipeWriter) {
	defer c.inFlight.Delete(req.RequestId)
	defer bodyReader.Close()

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, bodyReader)
	if err != nil {
		_ = bodyWriter.Close()
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
		_ = bodyWriter.CloseWithError(err)
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 502, Message: "local request failed"}}})
		return
	}
	defer resp.Body.Close()

	headers := tunnel.HeadersToMap(resp.Header, nil)
	msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ResStart{ResStart: &tunnelpb.ResponseStart{
		RequestId: req.RequestId,
		Status:    int32(resp.StatusCode),
		Headers:   headers,
	}}}
	if err := c.send(msg); err != nil {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 502, Message: "failed to send response headers"}}})
		return
	}
	if err := c.streamResponseBody(req.RequestId, resp.Body); err != nil {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 502, Message: "failed to stream response"}}})
		return
	}
}

func (c *Client) handleRemoteError(msg *tunnelpb.Error) {
	v, ok := c.inFlight.Load(msg.RequestId)
	if !ok {
		return
	}
	state := v.(*inFlightRequest)
	// Propagate edge-side aborts into the local HTTP request body reader.
	_ = state.bodyWriter.CloseWithError(errors.New(msg.Message))
	c.inFlight.Delete(msg.RequestId)
}

func (c *Client) handleBodyChunk(chunk *tunnelpb.RequestBodyChunk) {
	v, ok := c.inFlight.Load(chunk.RequestId)
	if !ok {
		return
	}
	state := v.(*inFlightRequest)
	if len(chunk.Data) > 0 {
		if _, err := state.bodyWriter.Write(chunk.Data); err != nil {
			_ = state.bodyWriter.CloseWithError(err)
			c.inFlight.Delete(chunk.RequestId)
			return
		}
	}
	if chunk.End {
		// Closing the pipe signals EOF to the local service request body.
		_ = state.bodyWriter.Close()
		c.inFlight.Delete(chunk.RequestId)
	}
}

func (c *Client) streamResponseBody(requestID string, body io.Reader) error {
	buf := make([]byte, streamChunkSize)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ResBody{ResBody: &tunnelpb.ResponseBodyChunk{
				RequestId: requestID,
				Data:      append([]byte(nil), buf[:n]...),
				End:       err == io.EOF,
			}}}
			if sendErr := c.send(msg); sendErr != nil {
				return sendErr
			}
		}

		if errors.Is(err, io.EOF) {
			if n == 0 {
				// Send an explicit terminal frame so the edge can complete
				// responses that end on an empty read.
				msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ResBody{ResBody: &tunnelpb.ResponseBodyChunk{
					RequestId: requestID,
					End:       true,
				}}}
				if sendErr := c.send(msg); sendErr != nil {
					return sendErr
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
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
