package warp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
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
	proxyURL *url.URL
	h2Trans  *http2.Transport
	h1Trans  *http.Transport
}

func newUTLSTransport(pu *url.URL) http.RoundTripper {
	return &utlsTransport{
		proxyURL: pu,
		h2Trans:  &http2.Transport{},
		h1Trans: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

func (t *utlsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return http.DefaultTransport.RoundTrip(req)
	}

	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second}
	ctx := req.Context()

	var conn net.Conn
	var err error

	if t.proxyURL != nil {
		slog.Debug("Warp AI: Dialing proxy", "proxy", t.proxyURL.Host)
		conn, err = dialer.DialContext(ctx, "tcp", t.proxyURL.Host)
		if err != nil {
			return nil, fmt.Errorf("proxy dial failed: %w", err)
		}

		slog.Debug("Warp AI: Sending CONNECT", "addr", addr)
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if t.proxyURL.User != nil {
			user := t.proxyURL.User.Username()
			pass, _ := t.proxyURL.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
		}
		connectReq += "\r\n"
		conn.Write([]byte(connectReq))

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy connect failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy connect status: %d", resp.StatusCode)
		}
		slog.Debug("Warp AI: Proxy CONNECT successful")
	} else {
		slog.Debug("Warp AI: Dialing direct", "addr", addr)
		conn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
	}

	// TLS Handshake
	host, _, _ := net.SplitHostPort(addr)
	slog.Debug("Warp AI: Starting uTLS handshake", "host", host, "addr", addr)
	config := &utls.Config{
		ServerName: host,
		NextProtos: []string{"http/1.1"},
	}

	// Create a spec based on Chrome 120 but filter out H2 from ALPN
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_120)
	if err == nil {
		for i, ext := range spec.Extensions {
			if alpn, ok := ext.(*utls.ALPNExtension); ok {
				alpn.AlpnProtocols = []string{"http/1.1"}
				spec.Extensions[i] = alpn
			}
		}
	}

	uconn := utls.UClient(conn, config, utls.HelloCustom)
	if err == nil {
		uconn.ApplyPreset(&spec)
	}
	if err := uconn.Handshake(); err != nil {
		conn.Close()
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

	// Use http1 Transport with a custom dialer that returns the already established conn
	// Note: for simpler implementation without complex connection pooling,
	// we just use a one-off transport.
	h1 := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return uconn, nil
		},
	}
	return h1.RoundTrip(req)
}
