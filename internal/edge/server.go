package edge

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"outbound/internal/tunnel"
	tunnelpb "outbound/proto"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
)

const (
	streamChunkSize      = 64 * 1024
	responseChunkBufSize = 16
)

var streamBufPool = sync.Pool{
	New: func() any { b := make([]byte, streamChunkSize); return &b },
}

// Session represents a single agent connection and its active request state.
type Session struct {
	agentID  string
	services map[string]uint16
	stream   tunnelpb.TunnelService_ConnectServer

	// sendMu ensures messages on the gRPC stream do not interleave.
	sendMu sync.Mutex

	pendingMu        sync.Mutex
	pendingResponses map[string]*pendingResponse
}

// pendingResponse tracks the response for a single request id.
type pendingResponse struct {
	// startCh receives exactly one ResponseStart per request_id.
	startCh chan *tunnelpb.ResponseStart
	// chunkCh receives zero or more ResponseBodyChunk frames until End=true.
	chunkCh chan *tunnelpb.ResponseBodyChunk
	failCh  chan error
	doneCtx context.Context
	stop    context.CancelFunc
}

// ServerConfig holds configurable parameters for the edge server.
type ServerConfig struct {
	RequestTimeout    time.Duration
	MaxRequestBody    int64
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	AuthSecret        string
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
	if config.KeepaliveInterval <= 0 {
		config.KeepaliveInterval = 15 * time.Second
	}
	if config.KeepaliveTimeout <= 0 {
		config.KeepaliveTimeout = 5 * time.Second
	}
	return &Server{
		config:   config,
		sessions: make(map[string]*Session),
	}
}

// Connect handles a single bidirectional tunnel connection from an agent.
func (s *Server) Connect(stream grpc.BidiStreamingServer[tunnelpb.TunnelMessage, tunnelpb.TunnelMessage]) error {
	session := &Session{
		services:         map[string]uint16{},
		stream:           stream,
		pendingResponses: make(map[string]*pendingResponse),
	}
	inboundMsgCh := make(chan *tunnelpb.TunnelMessage, 1)
	recvErrCh := make(chan error, 1)
	go func() {
		// Keep stream.Recv isolated in its own goroutine so keepalive and dispatch
		// never block behind a slow or stalled Recv call.
		for {
			msg, err := stream.Recv()
			if err != nil {
				select {
				case recvErrCh <- err:
				default:
				}
				return
			}
			select {
			case inboundMsgCh <- msg:
			case <-stream.Context().Done():
				return
			}
		}
	}()

	keepaliveTicker := time.NewTicker(s.config.KeepaliveInterval)
	defer keepaliveTicker.Stop()

	var (
		waitingForPong     bool
		lastPingTimestamp  int64
		pongTimer          *time.Timer
		pongDeadlineSignal <-chan time.Time
	)

	clearPongDeadline := func() {
		if pongTimer == nil {
			pongDeadlineSignal = nil
			return
		}
		// Stop+drain prevents a stale timer event from firing after Reset.
		if !pongTimer.Stop() {
			select {
			case <-pongTimer.C:
			default:
			}
		}
		pongDeadlineSignal = nil
	}
	defer clearPongDeadline()

	for {
		select {
		case err := <-recvErrCh:
			s.detachSession(session)
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-keepaliveTicker.C:
			if session.agentID == "" || waitingForPong {
				continue
			}
			now := time.Now().UnixMilli()
			ping := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_Ping{Ping: &tunnelpb.Ping{TsUnixMs: now}}}
			if err := s.send(session, ping); err != nil {
				s.detachSession(session)
				return err
			}
			lastPingTimestamp = now
			waitingForPong = true
			if pongTimer == nil {
				pongTimer = time.NewTimer(s.config.KeepaliveTimeout)
			} else {
				if !pongTimer.Stop() {
					select {
					case <-pongTimer.C:
					default:
					}
				}
				pongTimer.Reset(s.config.KeepaliveTimeout)
			}
			pongDeadlineSignal = pongTimer.C
		case <-pongDeadlineSignal:
			s.detachSession(session)
			return errors.New("keepalive timeout waiting for pong")
		case msg := <-inboundMsgCh:
			switch payload := msg.Msg.(type) {
			case *tunnelpb.TunnelMessage_Pong:
				if waitingForPong && payload.Pong.GetTsUnixMs() == lastPingTimestamp {
					waitingForPong = false
					clearPongDeadline()
				}
			default:
				if err := s.handleMessage(session, msg); err != nil {
					if errors.Is(err, errRegisterRejected) {
						return nil
					}
					s.detachSession(session)
					return err
				}
			}
		}
	}
}

var errRegisterRejected = errors.New("register rejected")

func (s *Server) handleMessage(session *Session, msg *tunnelpb.TunnelMessage) error {
	switch payload := msg.Msg.(type) {
	case *tunnelpb.TunnelMessage_Register:
		req := payload.Register
		if session.agentID != "" {
			// Ignore duplicate RegisterRequest on an already-registered session.
			return nil
		}
		if s.config.AuthSecret != "" && subtle.ConstantTimeCompare([]byte(req.GetToken()), []byte(s.config.AuthSecret)) != 1 {
			ack := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_RegisterAck{RegisterAck: &tunnelpb.RegisterAck{Ok: false, Message: "unauthorized"}}}
			if err := s.send(session, ack); err != nil {
				return err
			}
			return errRegisterRejected
		}
		session.agentID = req.AgentId
		services := map[string]uint16{}
		for _, service := range req.Services {
			services[service.Name] = uint16(service.Port)
		}
		session.services = services
		s.attachSession(session)
		ack := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_RegisterAck{RegisterAck: &tunnelpb.RegisterAck{Ok: true}}}
		return s.send(session, ack)
	case *tunnelpb.TunnelMessage_ResStart:
		s.dispatchResponseStart(session, payload.ResStart)
	case *tunnelpb.TunnelMessage_ResBody:
		s.dispatchResponseChunk(session, payload.ResBody)
	case *tunnelpb.TunnelMessage_Error:
		s.dispatchError(session, payload.Error)
	}
	return nil
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

	// Fail all pending responses immediately so in-flight HTTP handlers get a
	// 502 rather than hanging until RequestTimeout fires.
	session.pendingMu.Lock()
	pending := session.pendingResponses
	session.pendingResponses = make(map[string]*pendingResponse)
	session.pendingMu.Unlock()

	for _, p := range pending {
		p.stop()
		select {
		case p.failCh <- errors.New("agent disconnected"):
		default:
		}
	}
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

	pending := s.createPendingResponse(session, requestID)
	defer s.removePendingResponse(session, requestID)

	defer r.Body.Close()

	droppedHeaders := map[string]struct{}{strings.ToLower(tunnel.HeaderAgent): {}, strings.ToLower(tunnel.HeaderService): {}}
	requestStartMsg := &tunnelpb.TunnelMessage{Msg: &tunnelpb.TunnelMessage_ReqStart{ReqStart: &tunnelpb.RequestStart{
		RequestId: requestID,
		AgentId:   agentID,
		Service:   service,
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Headers:   tunnel.HeadersToMap(r.Header, droppedHeaders),
	}}}
	if err := s.send(session, requestStartMsg); err != nil {
		http.Error(w, "agent send failed", http.StatusBadGateway)
		return
	}
	if err := s.streamRequestBody(session, requestID, r.Body); err != nil {
		var sizeErr *requestBodyTooLargeError
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

	requestTimeoutTimer := time.NewTimer(s.config.RequestTimeout)
	defer requestTimeoutTimer.Stop()

	headersWritten := false
	select {
	case resp := <-pending.startCh:
		for key, value := range resp.Headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(int(resp.Status))
		headersWritten = true
	case err := <-pending.failCh:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case <-requestTimeoutTimer.C:
		http.Error(w, "agent response timeout", http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		return
	}

	for {
		select {
		case chunk := <-pending.chunkCh:
			if len(chunk.Data) > 0 {
				_, _ = w.Write(chunk.Data)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if chunk.End {
				return
			}
		case err := <-pending.failCh:
			if !headersWritten {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		case <-requestTimeoutTimer.C:
			if !headersWritten {
				http.Error(w, "agent response timeout", http.StatusGatewayTimeout)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

type requestBodyTooLargeError struct{}

func (e *requestBodyTooLargeError) Error() string {
	return "request body too large"
}

func (s *Server) streamRequestBody(session *Session, requestID string, requestBody io.Reader) error {
	bufPtr := streamBufPool.Get().(*[]byte)
	defer streamBufPool.Put(bufPtr)
	buf := *bufPtr
	var totalBytes int64
	for {
		n, err := requestBody.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if totalBytes > s.config.MaxRequestBody {
				return &requestBodyTooLargeError{}
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
				// Explicit terminal frame for EOF-on-empty-read.
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

func (s *Server) createPendingResponse(session *Session, requestID string) *pendingResponse {
	doneCtx, stop := context.WithCancel(context.Background())
	pending := &pendingResponse{
		startCh: make(chan *tunnelpb.ResponseStart, 1),
		chunkCh: make(chan *tunnelpb.ResponseBodyChunk, responseChunkBufSize),
		failCh:  make(chan error, 1),
		doneCtx: doneCtx,
		stop:    stop,
	}
	session.pendingMu.Lock()
	session.pendingResponses[requestID] = pending
	session.pendingMu.Unlock()
	return pending
}

func (s *Server) removePendingResponse(session *Session, requestID string) {
	session.pendingMu.Lock()
	pending := session.pendingResponses[requestID]
	delete(session.pendingResponses, requestID)
	session.pendingMu.Unlock()
	if pending != nil {
		pending.stop()
	}
}

func (s *Server) dispatchResponseStart(session *Session, resp *tunnelpb.ResponseStart) {
	pending := s.lookupPendingResponse(session, resp.RequestId)
	if pending == nil {
		return
	}
	select {
	case pending.startCh <- resp:
	case <-pending.doneCtx.Done():
	default:
	}
}

func (s *Server) dispatchResponseChunk(session *Session, chunk *tunnelpb.ResponseBodyChunk) {
	session.pendingMu.Lock()
	pending := session.pendingResponses[chunk.RequestId]
	if pending == nil {
		session.pendingMu.Unlock()
		return
	}
	// Keep lookup + overflow handling atomic under pendingMu so a concurrent
	// remove path cannot race this dispatch.
	select {
	case pending.chunkCh <- chunk:
		session.pendingMu.Unlock()
		return
	case <-pending.doneCtx.Done():
		session.pendingMu.Unlock()
		return
	default:
	}
	// Buffer full: remove under lock, then signal outside lock.
	delete(session.pendingResponses, chunk.RequestId)
	session.pendingMu.Unlock()
	pending.stop()
	select {
	case pending.failCh <- errors.New("response buffer overflow"):
	default:
	}
}

func (s *Server) dispatchError(session *Session, errMsg *tunnelpb.Error) {
	pending := s.lookupPendingResponse(session, errMsg.RequestId)
	if pending == nil {
		return
	}
	select {
	case pending.failCh <- fmt.Errorf("agent error: %s", errMsg.Message):
	case <-pending.doneCtx.Done():
	default:
	}
}

func (s *Server) lookupPendingResponse(session *Session, requestID string) *pendingResponse {
	session.pendingMu.Lock()
	pending := session.pendingResponses[requestID]
	session.pendingMu.Unlock()
	return pending
}
