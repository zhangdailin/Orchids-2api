package warp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

type utlsTransport struct {
	proxyFunc func(*http.Request) (*url.URL, error)
	h2Trans   *http2.Transport
	h1Trans   *http.Transport
}

func newUTLSTransport(proxyFunc func(*http.Request) (*url.URL, error)) http.RoundTripper {
	return &utlsTransport{
		proxyFunc: proxyFunc,
		h2Trans:   &http2.Transport{},
		h1Trans: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			Proxy:               proxyFunc,
		},
	}
}

// bufferedConn wraps a net.Conn with a bufio.Reader so that any data already
// buffered by the reader (e.g. after reading the CONNECT response) is not lost
// when the connection is handed off to the TLS layer.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.br.Read(b)
}

func (t *utlsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.h1Trans.RoundTrip(req)
	}

	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second}
	ctx := req.Context()

	var tlsConn net.Conn // the connection to pass to TLS (may be buffered)
	var err error

	var proxyURL *url.URL
	if t.proxyFunc != nil {
		parsed, err := t.proxyFunc(req)
		if err != nil {
			return nil, err
		}
		proxyURL = parsed
	}

	if proxyURL != nil {
		slog.Debug("Warp AI: Dialing proxy", "proxy", proxyURL.Host)
		conn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
		if err != nil {
			return nil, fmt.Errorf("proxy dial failed: %w", err)
		}

		slog.Debug("Warp AI: Sending CONNECT", "addr", addr)
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if proxyURL.User != nil {
			user := proxyURL.User.Username()
			pass, _ := proxyURL.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
		}
		connectReq += "\r\n"
		// Fix #11: check conn.Write error
		if _, err := conn.Write([]byte(connectReq)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy connect write failed: %w", err)
		}

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy connect failed: %w", err)
		}
		// Fix #12: close the CONNECT response body
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy connect status: %d", resp.StatusCode)
		}
		slog.Debug("Warp AI: Proxy CONNECT successful")

		// Fix #4: wrap conn with buffered reader to preserve any buffered data
		if br.Buffered() > 0 {
			tlsConn = &bufferedConn{Conn: conn, br: br}
		} else {
			tlsConn = conn
		}
	} else {
		slog.Debug("Warp AI: Dialing direct", "addr", addr)
		tlsConn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
	}

	// TLS Handshake
	host, _, _ := net.SplitHostPort(addr)
	slog.Debug("Warp AI: Starting uTLS handshake", "host", host, "addr", addr)
	config := &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2", "http/1.1"},
	}

	// Use Chrome 120 spec with natural ALPN (h2 + http/1.1) to avoid
	// CDN fingerprint detection that drops connections with mismatched ALPN.
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_120)

	uconn := utls.UClient(tlsConn, config, utls.HelloCustom)
	if err == nil {
		uconn.ApplyPreset(&spec)
	}
	if err := uconn.Handshake(); err != nil {
		tlsConn.Close()
		slog.Warn("Warp AI: uTLS handshake failed", "host", host, "error", err)
		return nil, fmt.Errorf("utls handshake: %w", err)
	}

	protocol := uconn.ConnectionState().NegotiatedProtocol
	slog.Debug("Warp AI: uTLS handshake successful", "protocol", protocol)

	if protocol == "h2" {
		// Use http2 Transport
		clientConn, err := t.h2Trans.NewClientConn(uconn)
		if err != nil {
			uconn.Close()
			return nil, fmt.Errorf("h2 new client conn: %w", err)
		}
		return clientConn.RoundTrip(req)
	}

	// Fix #13: use a one-off transport but ensure it is closed after use
	// to avoid leaking idle connections and goroutines.
	connUsed := false
	h1 := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if connUsed {
				return nil, fmt.Errorf("connection already consumed")
			}
			connUsed = true
			return uconn, nil
		},
		DisableKeepAlives: true,
	}
	resp, err := h1.RoundTrip(req)
	if err != nil {
		h1.CloseIdleConnections()
		uconn.Close()
		return nil, err
	}
	// Wrap the response body to close the transport when the body is fully read
	resp.Body = &transportClosingBody{ReadCloser: resp.Body, transport: h1}
	return resp, nil
}

// transportClosingBody wraps a response body and closes idle connections on
// the one-off transport when the body is closed, preventing resource leaks.
type transportClosingBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b *transportClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return err
}
