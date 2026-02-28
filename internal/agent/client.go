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
	requestBodyWriter *io.PipeWriter
}

const streamChunkSize = 64 * 1024

var streamBufPool = sync.Pool{
	New: func() any { b := make([]byte, streamChunkSize); return &b },
}

func NewClient(agentID string, services map[string]uint16) *Client {
	return &Client{
		agentID:  agentID,
		services: services,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *Client) Run(ctx context.Context, stream tunnelpb.TunnelService_ConnectClient) error {
	c.stream = stream
	sessionCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		c.failInFlight(errors.New("tunnel stream disconnected"))
	}()

	if err := c.register(); err != nil {
		return err
	}

	if err := c.waitForRegisterAck(); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		switch payload := msg.Msg.(type) {
		case *tunnelpb.TunnelMessage_ReqStart:
			c.handleRequestStart(sessionCtx, payload.ReqStart)
		case *tunnelpb.TunnelMessage_ReqBody:
			c.handleRequestBodyChunk(payload.ReqBody)
		case *tunnelpb.TunnelMessage_Error:
			c.handleRemoteError(payload.Error)
		case *tunnelpb.TunnelMessage_Ping:
			pong := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Pong{Pong: &tunnelpb.Pong{TsUnixMs: payload.Ping.TsUnixMs}}}
			_ = c.send(pong)
		}
	}
}

func (c *Client) waitForRegisterAck() error {
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

func (c *Client) handleRequestStart(ctx context.Context, req *tunnelpb.RequestStart) {
	port, ok := c.services[req.Service]
	if !ok {
		_ = c.send(&tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{RequestId: req.RequestId, Code: 404, Message: "service not found"}}})
		return
	}

	bodyReader, bodyWriter := io.Pipe()
	// Store before launching the goroutine so early ReqBody frames always find
	// their pipe writer.
	c.inFlight.Store(req.RequestId, &inFlightRequest{requestBodyWriter: bodyWriter})
	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, req.Path)
	go c.forwardRequest(ctx, req, targetURL, bodyReader, bodyWriter)
}

func (c *Client) forwardRequest(ctx context.Context, req *tunnelpb.RequestStart, targetURL string, requestBodyReader *io.PipeReader, requestBodyWriter *io.PipeWriter) {
	defer c.inFlight.Delete(req.RequestId)
	defer requestBodyReader.Close()

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, requestBodyReader)
	if err != nil {
		_ = requestBodyWriter.Close()
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
		_ = requestBodyWriter.CloseWithError(err)
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
	value, ok := c.inFlight.Load(msg.RequestId)
	if !ok {
		return
	}
	requestState := value.(*inFlightRequest)
	_ = requestState.requestBodyWriter.CloseWithError(errors.New(msg.Message))
	c.inFlight.Delete(msg.RequestId)
}

func (c *Client) handleRequestBodyChunk(chunk *tunnelpb.RequestBodyChunk) {
	value, ok := c.inFlight.Load(chunk.RequestId)
	if !ok {
		return
	}
	requestState := value.(*inFlightRequest)
	if len(chunk.Data) > 0 {
		if _, err := requestState.requestBodyWriter.Write(chunk.Data); err != nil {
			_ = requestState.requestBodyWriter.CloseWithError(err)
			c.inFlight.Delete(chunk.RequestId)
			return
		}
	}
	if chunk.End {
		_ = requestState.requestBodyWriter.Close()
		c.inFlight.Delete(chunk.RequestId)
	}
}

func (c *Client) streamResponseBody(requestID string, body io.Reader) error {
	bufPtr := streamBufPool.Get().(*[]byte)
	defer streamBufPool.Put(bufPtr)
	buf := *bufPtr
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
				// Explicit terminal frame for EOF-on-empty-read.
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

func (c *Client) failInFlight(err error) {
	c.inFlight.Range(func(key, value any) bool {
		state, ok := value.(*inFlightRequest)
		if ok {
			_ = state.requestBodyWriter.CloseWithError(err)
		}
		c.inFlight.Delete(key)
		return true
	})
}
