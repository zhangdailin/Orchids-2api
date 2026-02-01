package upstream

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSPool manages a pool of WebSocket connections
type WSPool struct {
	connections chan *websocket.Conn
	factory     func() (*websocket.Conn, error)
	minIdle     int
	maxSize     int
	mu          sync.RWMutex
	closed      bool
	done        chan struct{}
	closeOnce   sync.Once
	wg          sync.WaitGroup
}

// NewWSPool creates a new WebSocket connection pool
func NewWSPool(factory func() (*websocket.Conn, error), minIdle, maxSize int) *WSPool {
	pool := &WSPool{
		connections: make(chan *websocket.Conn, maxSize),
		factory:     factory,
		minIdle:     minIdle,
		maxSize:     maxSize,
		done:        make(chan struct{}),
	}

	// Pre-warm connections
	pool.wg.Add(1)
	go pool.warmUp()
	pool.wg.Add(1)
	go pool.maintainMinIdle()

	return pool
}

// Get retrieves a connection from the pool or creates a new one
func (p *WSPool) Get(ctx context.Context) (*websocket.Conn, error) {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrPoolClosed
	}
	p.mu.RUnlock()

	timer := time.NewTimer(100 * time.Millisecond)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	select {
	case conn := <-p.connections:
		if p.isHealthy(conn) {
			return conn, nil
		}
		conn.Close()
		// Fall through to create new
	case <-timer.C:
		// No idle connection available, create new
	case <-p.done:
		return nil, ErrPoolClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return p.factory()
}

// Put returns a connection to the pool
func (p *WSPool) Put(conn *websocket.Conn) {
	p.mu.RLock()
	closed := p.closed
	p.mu.RUnlock()

	if closed || conn == nil || !p.isHealthy(conn) {
		if conn != nil {
			conn.Close()
		}
		return
	}

	select {
	case p.connections <- conn:
		// Successfully returned to pool
	default:
		// Pool is full, close the connection
		conn.Close()
	}
}

// warmUp pre-creates minimum idle connections
func (p *WSPool) warmUp() {
	defer p.wg.Done()
	for i := 0; i < p.minIdle; i++ {
		select {
		case <-p.done:
			return
		default:
		}
		conn, err := p.factory()
		if err != nil {
			continue
		}
		select {
		case p.connections <- conn:
		case <-p.done:
			conn.Close()
			return
		default:
			conn.Close()
		}
	}
}

// maintainMinIdle ensures minimum number of idle connections
func (p *WSPool) maintainMinIdle() {
	defer p.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-p.done:
			return
		}
		p.mu.RLock()
		if p.closed {
			p.mu.RUnlock()
			return
		}
		p.mu.RUnlock()

		idle := len(p.connections)
		if idle < p.minIdle {
			needed := p.minIdle - idle
			for i := 0; i < needed; i++ {
				conn, err := p.factory()
				if err != nil {
					continue
				}
				select {
				case p.connections <- conn:
				case <-p.done:
					conn.Close()
					return
				default:
					conn.Close()
				}
			}
		}
	}
}

// isHealthy checks if a connection is still alive
func (p *WSPool) isHealthy(conn *websocket.Conn) bool {
	if conn == nil {
		return false
	}
	err := conn.WriteControl(
		websocket.PingMessage,
		[]byte{},
		time.Now().Add(1*time.Second),
	)
	return err == nil
}

// Close closes the pool and all connections
func (p *WSPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	p.closeOnce.Do(func() {
		close(p.done)
	})
	p.wg.Wait()

	for {
		select {
		case conn := <-p.connections:
			if conn != nil {
				conn.Close()
			}
		default:
			return
		}
	}
}

// Stats returns pool statistics
func (p *WSPool) Stats() PoolStats {
	return PoolStats{
		Idle:    len(p.connections),
		MinIdle: p.minIdle,
		MaxSize: p.maxSize,
	}
}

// PoolStats contains pool statistics
type PoolStats struct {
	Idle    int
	MinIdle int
	MaxSize int
}

var ErrPoolClosed = fmt.Errorf("connection pool is closed")
