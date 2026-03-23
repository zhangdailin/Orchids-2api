package warp

import (
	"net/http"
	"net/url"
	"time"
)

// warpTransport keeps the proxy function inspectable in tests while delegating
// actual I/O to the standard library transport. This mirrors CodeFreeMax's
// plain gclient usage while keeping the transport path minimal.
type warpTransport struct {
	base      *http.Transport
	proxyFunc func(*http.Request) (*url.URL, error)
}

func newWarpTransport(proxyFunc func(*http.Request) (*url.URL, error)) *warpTransport {
	if proxyFunc == nil {
		proxyFunc = http.ProxyFromEnvironment
	}

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = proxyFunc
	base.MaxIdleConns = 100
	base.MaxIdleConnsPerHost = 20
	base.MaxConnsPerHost = 50
	base.IdleConnTimeout = 90 * time.Second

	return &warpTransport{
		base:      base,
		proxyFunc: proxyFunc,
	}
}

func (t *warpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req)
}

func (t *warpTransport) CloseIdleConnections() {
	if t != nil && t.base != nil {
		t.base.CloseIdleConnections()
	}
}
