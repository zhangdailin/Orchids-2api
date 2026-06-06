package util

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestDialHTTPSProxyAwareSupportsSOCKS5(t *testing.T) {
	target := startEchoListener(t)
	proxy := startSOCKS5Proxy(t)

	proxyURL, err := url.Parse("socks5://" + proxy.Addr().String())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	proxyFunc := func(*http.Request) (*url.URL, error) {
		return proxyURL, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, targetHost, err := dialHTTPSProxyAware(ctx, "tcp", target.Addr().String(), proxyFunc)
	if err != nil {
		t.Fatalf("dialHTTPSProxyAware() error = %v", err)
	}
	defer conn.Close()
	if targetHost == "" {
		t.Fatal("targetHost is empty")
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through proxy: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through proxy: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo=%q want ping", string(buf))
	}
}

func startEchoListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln
}

func startSOCKS5Proxy(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5ProxyConn(conn)
		}
	}()
	return ln
}

func handleSOCKS5ProxyConn(conn net.Conn) {
	defer conn.Close()
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	if req[0] != 5 || req[1] != 1 {
		return
	}
	host := ""
	switch req[3] {
	case 1:
		addr := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	case 3:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return
		}
		addr := make([]byte, int(size[0]))
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = string(addr)
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		done <- struct{}{}
	}()
	<-done
}
