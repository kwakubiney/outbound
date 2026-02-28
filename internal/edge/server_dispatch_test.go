package edge

import (
	"testing"
	"time"

	tunnelpb "outbound/proto"
)

func TestDispatchResponseChunkBufferOverflowFailsPending(t *testing.T) {
	server := NewServer(ServerConfig{})
	session := &Session{pendingResponses: make(map[string]*pendingResponse)}
	requestID := "req-overflow"
	pending := server.createPendingResponse(session, requestID)

	for i := 0; i < responseChunkBufSize; i++ {
		pending.chunkCh <- &tunnelpb.ResponseBodyChunk{RequestId: requestID, Data: []byte("x")}
	}

	server.dispatchResponseChunk(session, &tunnelpb.ResponseBodyChunk{RequestId: requestID, Data: []byte("overflow")})

	select {
	case err := <-pending.failCh:
		if err == nil || err.Error() != "response buffer overflow" {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected overflow error")
	}

	if got := server.lookupPendingResponse(session, requestID); got != nil {
		t.Fatal("pending response should be removed after overflow")
	}
}
