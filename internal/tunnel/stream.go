package tunnel

import (
	"crypto/rand"
	"encoding/hex"
)

// NewRequestID returns an identifier for a single HTTP request/response flow
// multiplexed over a shared gRPC stream.
func NewRequestID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
