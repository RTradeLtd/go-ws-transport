// +build !js

package websocket

import (
	"io"
	"net"
	"sync"
	"time"

	ws "github.com/gorilla/websocket"
	"go.uber.org/multierr"
)

// Conn implements net.Conn interface for gorilla/websocket.
type Conn struct {
	*ws.Conn
	DefaultMessageType int
	reader             io.Reader
	closeOnce          sync.Once
	mux                sync.RWMutex
}

func (c *Conn) Read(b []byte) (int, error) {
	c.mux.RLock()
	if c.reader == nil {
		if err := c.prepNextReader(); err != nil {
			c.mux.RUnlock()
			return 0, err
		}
	}

	for {
		n, err := c.reader.Read(b)
		switch err {
		case io.EOF:
			c.reader = nil

			if n > 0 {
				c.mux.RUnlock()
				return n, nil
			}

			if err := c.prepNextReader(); err != nil {
				c.mux.RUnlock()
				return 0, err
			}

			// explicitly looping
		default:
			c.mux.RUnlock()
			return n, err
		}
	}
}

func (c *Conn) prepNextReader() error {
	t, r, err := c.Conn.NextReader()
	if err != nil {
		if wserr, ok := err.(*ws.CloseError); ok {
			if wserr.Code == 1000 || wserr.Code == 1005 {
				return io.EOF
			}
		}
		return err
	}

	if t == ws.CloseMessage {
		return io.EOF
	}

	c.reader = r
	return nil
}

func (c *Conn) Write(b []byte) (n int, err error) {
	c.mux.Lock()
	if err := c.Conn.WriteMessage(c.DefaultMessageType, b); err != nil {
		c.mux.Unlock()
		return 0, err
	}
	c.mux.Unlock()
	return len(b), nil
}

// Close closes the connection. Only the first call to Close will receive the
// close error, subsequent and concurrent calls will return nil.
// This method is thread-safe.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err1 := c.Conn.WriteControl(
			ws.CloseMessage,
			ws.FormatCloseMessage(ws.CloseNormalClosure, "closed"),
			time.Now().Add(GracefulCloseTimeout),
		)
		err2 := c.Conn.Close()
		err = multierr.Combine(err1, err2)
	})
	return err
}

func (c *Conn) LocalAddr() net.Addr {
	return NewAddr(c.Conn.LocalAddr().String())
}

func (c *Conn) RemoteAddr() net.Addr {
	return NewAddr(c.Conn.RemoteAddr().String())
}

func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}

	return c.SetWriteDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mux.Lock()
	err := c.Conn.SetWriteDeadline(t)
	c.mux.Unlock()
	return err
}

// NewConn creates a Conn given a regular gorilla/websocket Conn.
func NewConn(raw *ws.Conn) *Conn {
	return &Conn{
		Conn:               raw,
		DefaultMessageType: ws.BinaryMessage,
	}
}
