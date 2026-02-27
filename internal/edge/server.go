package edge

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc"
	"io"
	"net/http"
	"outbound/internal/tunnel"
	tunnelpb "outbound/proto"
	"strings"
	"sync"
	"time"
)

const (
	streamChunkSize      = 64 * 1024
	responseChunkBufSize = 16
)

// Session represents a single agent connection and its active request state.
type Session struct {
	agentID  string
	services map[string]uint16
	stream   tunnelpb.TunnelService_ConnectServer

	// sendMu ensures messages on the gRPC stream do not interleave.
	sendMu sync.Mutex

	mu        sync.Mutex
	responses map[string]*pendingResponse
}

// pendingResponse tracks the response for a single request id.
type pendingResponse struct {
	// headersCh receives exactly one ResponseStart per request_id.
	headersCh chan *tunnelpb.ResponseStart
	// bodyCh receives zero or more ResponseBodyChunk frames until End=true.
	bodyCh chan *tunnelpb.ResponseBodyChunk
	errCh  chan error
}

// ServerConfig holds configurable parameters for the edge server.
type ServerConfig struct {
	RequestTimeout time.Duration
	MaxRequestBody int64
}

// Server is the edge entry point that accepts HTTP and forwards over gRPC.
type Server struct {
	tunnelpb.UnimplementedTunnelServiceServer

	config ServerConfig

	sessions   map[string]*Session
	sessionsMu sync.RWMutex
}

func NewServer(config ServerConfig) *Server {
	// Apply defaults
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.MaxRequestBody <= 0 {
		config.MaxRequestBody = 10 * 1024 * 1024 // 10MB
	}
	return &Server{
		config:   config,
		sessions: make(map[string]*Session),
	}
}

// Connect handles a single bidirectional tunnel connection from an agent.
func (s *Server) Connect(stream grpc.BidiStreamingServer[tunnelpb.TunnelMessage, tunnelpb.TunnelMessage]) error {
	session := &Session{
		services:  map[string]uint16{},
		stream:    stream,
		responses: make(map[string]*pendingResponse),
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			s.detachSession(session)
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		s.handleMessage(session, msg)
	}
}

func (s *Server) handleMessage(session *Session, msg *tunnelpb.TunnelMessage) {
	switch payload := msg.Msg.(type) {
	case *tunnelpb.TunnelMessage_Register:
		req := payload.Register
		session.agentID = req.AgentId
		services := map[string]uint16{}
		for _, service := range req.Services {
			services[service.Name] = uint16(service.Port)
		}
		session.services = services
		s.attachSession(session)
		ack := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_RegisterAck{RegisterAck: &tunnelpb.RegisterAck{Ok: true}}}
		_ = s.send(session, ack)
	case *tunnelpb.TunnelMessage_ResStart:
		s.dispatchResponseStart(session, payload.ResStart)
	case *tunnelpb.TunnelMessage_ResBody:
		s.dispatchResponseChunk(session, payload.ResBody)
	case *tunnelpb.TunnelMessage_Error:
		s.dispatchError(session, payload.Error)
	case *tunnelpb.TunnelMessage_Ping:
		pong := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Pong{Pong: &tunnelpb.Pong{TsUnixMs: payload.Ping.TsUnixMs}}}
		_ = s.send(session, pong)
	}
}

func (s *Server) attachSession(session *Session) {
	if session.agentID == "" {
		return
	}
	s.sessionsMu.Lock()
	s.sessions[session.agentID] = session
	s.sessionsMu.Unlock()
}

func (s *Server) detachSession(session *Session) {
	if session.agentID == "" {
		return
	}
	s.sessionsMu.Lock()
	if current, ok := s.sessions[session.agentID]; ok && current == session {
		delete(s.sessions, session.agentID)
	}
	s.sessionsMu.Unlock()
}

func (s *Server) send(session *Session, msg *tunnelpb.TunnelMessage) error {
	session.sendMu.Lock()
	defer session.sendMu.Unlock()
	return session.stream.Send(msg)
}

// ServeHTTP accepts public HTTP requests and forwards them to the agent tunnel.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	agentID := r.Header.Get(tunnel.HeaderAgent)
	service := r.Header.Get(tunnel.HeaderService)
	if agentID == "" || service == "" {
		http.Error(w, "missing outbound routing headers", http.StatusBadRequest)
		return
	}

	session := s.lookupSession(agentID)
	if session == nil {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, ok := session.services[service]; !ok {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	requestID, err := tunnel.NewRequestID()
	if err != nil {
		http.Error(w, "failed to create request id", http.StatusInternalServerError)
		return
	}
	if r.ContentLength > s.config.MaxRequestBody {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	pending := s.newPending(session, requestID)
	defer s.clearPending(session, requestID)

	defer r.Body.Close()

	// Drop routing headers so the local service never sees them.
	drop := map[string]struct{}{strings.ToLower(tunnel.HeaderAgent): {}, strings.ToLower(tunnel.HeaderService): {}}
	start := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ReqStart{ReqStart: &tunnelpb.RequestStart{
		RequestId: requestID,
		AgentId:   agentID,
		Service:   service,
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Headers:   tunnel.HeadersToMap(r.Header, drop),
	}}}
	if err := s.send(session, start); err != nil {
		http.Error(w, "agent send failed", http.StatusBadGateway)
		return
	}
	if err := s.streamRequestBody(session, requestID, r.Body); err != nil {
		var sizeErr *requestTooLargeError
		if errors.As(err, &sizeErr) {
			// Best-effort signal so the agent can stop any local work already
			// started for this request before the edge returns 413.
			_ = s.send(session, &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Error{Error: &tunnelpb.Error{
				RequestId: requestID,
				Code:      http.StatusRequestEntityTooLarge,
				Message:   "request body too large",
			}}})
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	timer := time.NewTimer(s.config.RequestTimeout)
	defer timer.Stop()

	headersWritten := false
	// Wait for response headers first so we can set status/headers exactly once
	// before streaming response body chunks.
	select {
	case resp := <-pending.headersCh:
		for key, value := range resp.Headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(int(resp.Status))
		headersWritten = true
	case err := <-pending.errCh:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case <-timer.C:
		http.Error(w, "agent response timeout", http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		return
	}

	for {
		select {
		case chunk := <-pending.bodyCh:
			if len(chunk.Data) > 0 {
				_, _ = w.Write(chunk.Data)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if chunk.End {
				return
			}
		case err := <-pending.errCh:
			if !headersWritten {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		case <-timer.C:
			// If headers are already sent, we cannot change status code at this point.
			if !headersWritten {
				http.Error(w, "agent response timeout", http.StatusGatewayTimeout)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

type requestTooLargeError struct{}

func (e *requestTooLargeError) Error() string {
	return "request body too large"
}

func (s *Server) streamRequestBody(session *Session, requestID string, body io.Reader) error {
	buf := make([]byte, streamChunkSize)
	var total int64
	for {
		n, err := body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > s.config.MaxRequestBody {
				return &requestTooLargeError{}
			}
			msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ReqBody{ReqBody: &tunnelpb.RequestBodyChunk{
				RequestId: requestID,
				Data:      append([]byte(nil), buf[:n]...),
				End:       err == io.EOF,
			}}}
			if sendErr := s.send(session, msg); sendErr != nil {
				return sendErr
			}
		}

		if errors.Is(err, io.EOF) {
			if n == 0 {
				// Send a terminal empty chunk when the final read is EOF with no bytes.
				// This keeps end-of-stream signaling explicit for the agent side.
				msg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ReqBody{ReqBody: &tunnelpb.RequestBodyChunk{
					RequestId: requestID,
					End:       true,
				}}}
				if sendErr := s.send(session, msg); sendErr != nil {
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

func (s *Server) lookupSession(agentID string) *Session {
	s.sessionsMu.RLock()
	session := s.sessions[agentID]
	s.sessionsMu.RUnlock()
	return session
}

func (s *Server) newPending(session *Session, requestID string) *pendingResponse {
	pr := &pendingResponse{
		headersCh: make(chan *tunnelpb.ResponseStart, 1),
		bodyCh:    make(chan *tunnelpb.ResponseBodyChunk, responseChunkBufSize),
		errCh:     make(chan error, 1),
	}
	session.mu.Lock()
	session.responses[requestID] = pr
	session.mu.Unlock()
	return pr
}

func (s *Server) clearPending(session *Session, requestID string) {
	session.mu.Lock()
	delete(session.responses, requestID)
	session.mu.Unlock()
}

func (s *Server) dispatchResponseStart(session *Session, resp *tunnelpb.ResponseStart) {
	pr := s.getPending(session, resp.RequestId)
	if pr == nil {
		return
	}
	pr.headersCh <- resp
}

func (s *Server) dispatchResponseChunk(session *Session, chunk *tunnelpb.ResponseBodyChunk) {
	pr := s.getPending(session, chunk.RequestId)
	if pr == nil {
		return
	}
	pr.bodyCh <- chunk
}

func (s *Server) dispatchError(session *Session, errMsg *tunnelpb.Error) {
	pr := s.getPending(session, errMsg.RequestId)
	if pr == nil {
		return
	}
	pr.errCh <- fmt.Errorf("agent error: %s", errMsg.Message)
}

func (s *Server) getPending(session *Session, requestID string) *pendingResponse {
	session.mu.Lock()
	pr := session.responses[requestID]
	session.mu.Unlock()
	return pr
}
