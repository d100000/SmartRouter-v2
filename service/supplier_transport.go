package service

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gorilla/websocket"
)

// Supplier endpoints are untrusted even when operator-managed channels may
// legitimately reach private networks. Never inherit a global SSRF bypass,
// insecure TLS setting, proxy, or cross-origin redirect for supplier traffic.
func ValidateSupplierURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("supplier upstream requires an HTTPS URL without credentials or query parameters")
	}
	protection := &common.SSRFProtection{ApplyIPFilterForDomain: true}
	if err := protection.ValidateURL(raw); err != nil {
		return errors.New("supplier upstream target is not allowed")
	}
	return nil
}

var supplierHTTPClient = sync.OnceValue(func() *http.Client {
	dialer := &protectedFetchDialer{
		resolver:    net.DefaultResolver,
		dialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		getProtection: func() (*common.SSRFProtection, bool, error) {
			return &common.SSRFProtection{ApplyIPFilterForDomain: true}, true, nil
		},
	}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dialer.DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 120 * time.Second},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
})

func GetSupplierHTTPClient() *http.Client { return supplierHTTPClient() }

// Supplier WebSockets use the same DNS pinning and address policy as HTTP.
// The dialer never inherits a proxy, insecure TLS option or redirect policy.
func SupplierWebSocketDialer(raw string) (*websocket.Dialer, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("supplier upstream requires a secure WebSocket URL")
	}
	u.Scheme, u.RawQuery = "https", ""
	if err := ValidateSupplierURL(u.String()); err != nil {
		return nil, err
	}
	dialer := &protectedFetchDialer{
		resolver:    net.DefaultResolver,
		dialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		getProtection: func() (*common.SSRFProtection, bool, error) {
			return &common.SSRFProtection{ApplyIPFilterForDomain: true}, true, nil
		},
	}
	return &websocket.Dialer{NetDialContext: dialer.DialContext, HandshakeTimeout: 30 * time.Second}, nil
}

// RedactUpstreamWebSocketCredential preserves JSON numbers and framing while
// removing usable upstream secrets from every string, including escaped error
// messages and session metadata. It does not persist a decoded response.
func RedactUpstreamWebSocketCredential(payload []byte, key string) []byte {
	if key == "" {
		return payload
	}
	var output bytes.Buffer
	last := 0
	for i := 0; i < len(payload); i++ {
		if payload[i] != '"' {
			continue
		}
		start := i
		for i++; i < len(payload); i++ {
			if payload[i] == '\\' {
				i++
				continue
			}
			if payload[i] == '"' {
				break
			}
		}
		if i >= len(payload) {
			break
		}
		var value string
		if common.Unmarshal(payload[start:i+1], &value) != nil || !strings.Contains(value, key) {
			continue
		}
		replacement, err := common.Marshal(strings.ReplaceAll(value, key, "[REDACTED]"))
		if err != nil {
			continue
		}
		output.Write(payload[last:start])
		output.Write(replacement)
		last = i + 1
	}
	if last == 0 {
		return payload
	}
	output.Write(payload[last:])
	return output.Bytes()
}
