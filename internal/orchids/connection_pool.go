package orchids

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type orchidsWSFactory func(context.Context) (*websocket.Conn, error)

type ConnectionPoolManager struct {
	pools sync.Map
}

var (
	connectionPoolManager     *ConnectionPoolManager
	connectionPoolManagerOnce sync.Once
)

func GetConnectionPoolManager() *ConnectionPoolManager {
	connectionPoolManagerOnce.Do(func() {
		connectionPoolManager = &ConnectionPoolManager{}
	})
	return connectionPoolManager
}

func (m *ConnectionPoolManager) GetConnection(key string, factory orchidsWSFactory) *OrchidsWebSocket {
	if key == "" {
		key = "orchids:default"
	}

	candidate := &OrchidsWebSocket{
		key:     key,
		factory: factory,
	}

	actual, loaded := m.pools.LoadOrStore(key, candidate)
	if loaded {
		ws := actual.(*OrchidsWebSocket)
		ws.setFactory(factory)
		return ws
	}

	return candidate
}

type OrchidsWebSocket struct {
	key       string
	requestMu sync.Mutex
	stateMu   sync.Mutex
	writeMu   sync.Mutex
	factory   orchidsWSFactory
	conn      *websocket.Conn
}

func (ws *OrchidsWebSocket) setFactory(factory orchidsWSFactory) {
	ws.stateMu.Lock()
	ws.factory = factory
	ws.stateMu.Unlock()
}

func (ws *OrchidsWebSocket) BeginRequest(ctx context.Context) error {
	ws.requestMu.Lock()
	if err := ws.ensureConn(ctx); err != nil {
		ws.requestMu.Unlock()
		return err
	}
	return nil
}

func (ws *OrchidsWebSocket) EndRequest() {
	ws.requestMu.Unlock()
}

func (ws *OrchidsWebSocket) SendRequest(request *OrchidsRequest) error {
	conn, err := ws.currentConn()
	if err != nil {
		return err
	}

	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return conn.WriteJSON(request)
}

func (ws *OrchidsWebSocket) ReadMessageWithTimeout(timeout time.Duration) ([]byte, error) {
	conn, err := ws.currentConn()
	if err != nil {
		return nil, err
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (ws *OrchidsWebSocket) Ping(deadline time.Time) error {
	conn, err := ws.currentConn()
	if err != nil {
		return err
	}

	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return conn.WriteControl(websocket.PingMessage, []byte{}, deadline)
}

func (ws *OrchidsWebSocket) Invalidate() error {
	ws.stateMu.Lock()
	conn := ws.conn
	ws.conn = nil
	ws.stateMu.Unlock()

	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (ws *OrchidsWebSocket) Close() error {
	return ws.Invalidate()
}

func (ws *OrchidsWebSocket) ensureConn(ctx context.Context) error {
	ws.stateMu.Lock()
	defer ws.stateMu.Unlock()

	if ws.conn != nil {
		return nil
	}
	if ws.factory == nil {
		return errors.New("orchids websocket factory is nil")
	}

	conn, err := ws.factory(ctx)
	if err != nil {
		return err
	}
	ws.conn = conn
	return nil
}

func (ws *OrchidsWebSocket) currentConn() (*websocket.Conn, error) {
	ws.stateMu.Lock()
	conn := ws.conn
	ws.stateMu.Unlock()

	if conn == nil {
		return nil, errors.New("orchids websocket connection is not initialized")
	}
	return conn, nil
}
