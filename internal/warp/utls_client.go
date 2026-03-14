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
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/util"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

type h2ConnEntry struct {
	mu      sync.Mutex
	conn    *http2.ClientConn
	created time.Time
}

const h2ConnMaxAge = 5 * time.Minute

type utlsTransport struct {
	proxyFunc func(*http.Request) (*url.URL, error)
	h2Trans   *http2.Transport
	h1Trans   *http.Transport
	h2Pool    sync.Map
}

func newAuthHTTPClient(timeout time.Duration, cfg *config.Config) *http.Client {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
		if cfg != nil && cfg.RequestTimeout > 0 {
			timeout = time.Duration(cfg.RequestTimeout) * time.Second
		}
	}

	var proxyFunc func(*http.Request) (*url.URL, error)
	if cfg != nil {
		proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: newUTLSTransport(proxyFunc),
	}
}

func (t *utlsTransport) CloseIdleConnections() {
	if t == nil {
		return
	}
	if t.h1Trans != nil {
		t.h1Trans.CloseIdleConnections()
	}
	t.h2Pool.Range(func(key, value interface{}) bool {
		entry, ok := value.(*h2ConnEntry)
		if !ok || entry == nil {
			t.h2Pool.Delete(key)
			return true
		}
		entry.mu.Lock()
		if entry.conn != nil {
			_ = entry.conn.Close()
			entry.conn = nil
		}
		entry.mu.Unlock()
		t.h2Pool.Delete(key)
		return true
	})
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

type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.br.Read(b)
}

func (t *utlsTransport) tryPooledH2(req *http.Request, addr string) (*http.Response, error) {
	val, ok := t.h2Pool.Load(addr)
	if !ok {
		return nil, nil
	}
	entry := val.(*h2ConnEntry)
	entry.mu.Lock()
	cc := entry.conn
	created := entry.created
	entry.mu.Unlock()

	if cc == nil || time.Since(created) > h2ConnMaxAge || !cc.CanTakeNewRequest() {
		t.h2Pool.Delete(addr)
		return nil, nil
	}
	resp, err := cc.RoundTrip(req)
	if err != nil {
		t.h2Pool.Delete(addr)
		return nil, nil
	}
	return resp, nil
}

func (t *utlsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.h1Trans.RoundTrip(req)
	}

	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	var proxyURL *url.URL
	if t.proxyFunc != nil {
		parsed, err := t.proxyFunc(req)
		if err != nil {
			return nil, err
		}
		proxyURL = parsed
	}

	if proxyURL == nil {
		if resp, err := t.tryPooledH2(req, addr); resp != nil || err != nil {
			return resp, err
		}
	}

	ctx := req.Context()
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	var tlsConn net.Conn
	var err error

	if proxyURL != nil {
		slog.Debug("Warp auth: dialing proxy", "proxy", proxyURL.Host)
		conn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
		if err != nil {
			return nil, fmt.Errorf("proxy dial failed: %w", err)
		}

		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if proxyURL.User != nil {
			user := proxyURL.User.Username()
			pass, _ := proxyURL.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
		}
		connectReq += "\r\n"
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
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy connect status: %d", resp.StatusCode)
		}
		if br.Buffered() > 0 {
			tlsConn = &bufferedConn{Conn: conn, br: br}
		} else {
			tlsConn = conn
		}
	} else {
		tlsConn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
	}

	host, _, _ := net.SplitHostPort(addr)
	config := &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2", "http/1.1"},
	}

	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_131)
	uconn := utls.UClient(tlsConn, config, utls.HelloCustom)
	if err == nil {
		uconn.ApplyPreset(&spec)
	}
	if err := uconn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}

	if uconn.ConnectionState().NegotiatedProtocol == "h2" {
		clientConn, err := t.h2Trans.NewClientConn(uconn)
		if err != nil {
			uconn.Close()
			return nil, fmt.Errorf("h2 new client conn: %w", err)
		}
		if proxyURL == nil {
			entry := &h2ConnEntry{conn: clientConn, created: time.Now()}
			if old, loaded := t.h2Pool.Swap(addr, entry); loaded {
				oldEntry := old.(*h2ConnEntry)
				oldEntry.mu.Lock()
				if oldEntry.conn != nil {
					_ = oldEntry.conn.Close()
					oldEntry.conn = nil
				}
				oldEntry.mu.Unlock()
			}
		}
		return clientConn.RoundTrip(req)
	}

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
	resp.Body = &transportClosingBody{ReadCloser: resp.Body, transport: h1}
	return resp, nil
}

type transportClosingBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b *transportClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return err
}
