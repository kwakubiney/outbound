package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"outbound/internal/tunnel"
	tunnelpb "outbound/proto"
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
	respCh chan *tunnelpb.ResponseStart
	errCh  chan error
}

// Server is the edge entry point that accepts HTTP and forwards over gRPC.
type Server struct {
	tunnelpb.UnimplementedTunnelServiceServer

	sessions   map[string]*Session
	sessionsMu sync.RWMutex
}

func NewServer() *Server {
	return &Server{
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
		s.dispatchResponse(session, payload.ResStart)
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
		http.Error(w, "agent unavailable", http.StatusBadGateway)
		return
	}
	if _, ok := session.services[service]; !ok {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}

	requestID, err := tunnel.NewRequestID()
	if err != nil {
		http.Error(w, "failed to create request id", http.StatusInternalServerError)
		return
	}

	pending := s.newPending(session, requestID)
	defer s.clearPending(session, requestID)

	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	// Drop routing headers so the local service never sees them.
	drop := map[string]struct{}{strings.ToLower(tunnel.HeaderAgent): {}, strings.ToLower(tunnel.HeaderService): {}}
	start := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ReqStart{ReqStart: &tunnelpb.RequestStart{
		RequestId: requestID,
		AgentId:   agentID,
		Service:   service,
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Headers:   tunnel.HeadersToMap(r.Header, drop),
		Body:      body,
	}}}
	if err := s.send(session, start); err != nil {
		http.Error(w, "agent send failed", http.StatusBadGateway)
		return
	}

	select {
	case resp := <-pending.respCh:
		for key, value := range resp.Headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(int(resp.Status))
		if len(resp.Body) > 0 {
			_, _ = w.Write(resp.Body)
		}
	case err := <-pending.errCh:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case <-time.After(10 * time.Second):
		http.Error(w, "agent response timeout", http.StatusGatewayTimeout)
		return
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
		respCh: make(chan *tunnelpb.ResponseStart, 1),
		errCh:  make(chan error, 1),
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

func (s *Server) dispatchResponse(session *Session, resp *tunnelpb.ResponseStart) {
	pr := s.getPending(session, resp.RequestId)
	if pr == nil {
		return
	}
	pr.respCh <- resp
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
