// Package tunnel provides helpers for forwarding HTTP requests over the gRPC tunnel.
package tunnel

import (
	"net/http"
	"strings"
)

const (
	HeaderAgent   = "X-Outbound-Agent"
	HeaderService = "X-Outbound-Service"
)

var hopByHopHeaders = map[string]struct{}{
	"connection":        {},
	"keep-alive":        {},
	"proxy-connection":  {},
	"transfer-encoding": {},
	"upgrade":           {},
	"te":                {},
	"trailer":           {},
}

// HeadersToMap flattens HTTP headers into a string map for tunneling.
// It drops hop-by-hop headers and any keys provided in dropKeys.
func HeadersToMap(h http.Header, dropKeys map[string]struct{}) map[string]string {
	out := make(map[string]string, len(h))
	for key, values := range h {
		lower := strings.ToLower(key)
		if _, drop := hopByHopHeaders[lower]; drop {
			continue
		}
		if dropKeys != nil {
			if _, drop := dropKeys[lower]; drop {
				continue
			}
		}
		out[key] = strings.Join(values, ",")
	}
	return out
}

// MapToHeaders restores a string map into HTTP headers, excluding hop-by-hop keys.
func MapToHeaders(dst http.Header, src map[string]string) {
	for key, value := range src {
		lower := strings.ToLower(key)
		if _, drop := hopByHopHeaders[lower]; drop {
			continue
		}
		dst.Set(key, value)
	}
}
