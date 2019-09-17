// +build js,wasm

package websocket

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

const (
	webSocketStateConnecting = 0
	webSocketStateOpen       = 1
	webSocketStateClosing    = 2
	webSocketStateClosed     = 3
)

var errConnectionClosed = errors.New("connection is closed")

// Conn implements net.Conn interface for WebSockets in js/wasm.
type Conn struct {
	js.Value
	messageHandler *js.Func
	closeHandler   *js.Func
	errorHandler   *js.Func
	mut            sync.Mutex
	currDataMut    sync.RWMutex
	currData       bytes.Buffer
	closeOnce      sync.Once
	closeSignal    chan struct{}
	dataSignal     chan struct{}
	localAddr      net.Addr
	remoteAddr     net.Addr
	firstErr       error
}

// NewConn creates a Conn given a regular js/wasm WebSocket Conn.
func NewConn(raw js.Value) *Conn {
	conn := &Conn{
		Value:       raw,
		closeSignal: make(chan struct{}),
		dataSignal:  make(chan struct{}, 1),
		localAddr:   NewAddr("0.0.0.0:0"),
		remoteAddr:  getRemoteAddr(raw),
	}
	// Force the JavaScript WebSockets API to use the ArrayBuffer type for
	// incoming messages instead of the Blob type. This is better for us because
	// ArrayBuffer can be converted to []byte synchronously but Blob cannot.
	conn.Set("binaryType", "arraybuffer")
	conn.setUpHandlers()
	return conn
}

func (c *Conn) Read(b []byte) (int, error) {
	if err := c.checkOpen(); err != nil {
		return c.readAfterErr(b)
	}

	for {
		c.currDataMut.RLock()
		n, _ := c.currData.Read(b)
		c.currDataMut.RUnlock()

		if n != 0 {
			// Data was ready. Return the number of bytes read.
			return n, nil
		}

		// There is no data ready to be read. Wait for more data or for the
		// connection to be closed.
		select {
		case <-c.dataSignal:
			continue
		case <-c.closeSignal:
			return c.readAfterErr(b)
		}
	}
}

// readAfterError reads from c.currData. If there is no more data left it
// returns c.firstErr if non-nil and otherwise returns io.EOF.
func (c *Conn) readAfterErr(b []byte) (int, error) {
	c.currDataMut.RLock()
	n, err := c.currData.Read(b)
	c.currDataMut.RUnlock()
	if n == 0 {
		if c.firstErr != nil {
			return 0, c.firstErr
		} else {
			return 0, io.EOF
		}
	}
	return n, err
}

// checkOpen returns an error if the connection is not open. Otherwise, it
// returns nil.
func (c *Conn) checkOpen() error {
	state := c.Get("readyState").Int()
	switch state {
	case webSocketStateClosed, webSocketStateClosing:
		return errConnectionClosed
	}
	return nil
}

func (c *Conn) Write(b []byte) (n int, err error) {
	if err := c.checkOpen(); err != nil {
		return 0, err
	}
	uint8Array := js.Global().Get("Uint8Array").New(len(b))
	for i := 0; i < len(b); i++ {
		uint8Array.SetIndex(i, b[i])
	}
	c.Call("send", uint8Array.Get("buffer"))
	return len(b), nil
}

// Close closes the connection. Only the first call to Close will receive the
// close error, subsequent and concurrent calls will return nil.
// This method is thread-safe.
func (c *Conn) Close() error {
	c.signalClose()
	c.Call("close")
	c.releaseHandlers()
	return nil
}

func (c *Conn) signalClose() {
	c.closeOnce.Do(func() {
		close(c.closeSignal)
	})
}

func (c *Conn) releaseHandlers() {
	c.mut.Lock()
	defer c.mut.Unlock()
	if c.messageHandler != nil {
		c.Call("removeEventListener", "message", *c.messageHandler)
		c.messageHandler.Release()
	}
	if c.closeHandler != nil {
		c.Call("removeEventListener", "close", *c.closeHandler)
		c.closeHandler.Release()
	}
	if c.errorHandler != nil {
		c.Call("removeEventListener", "error", *c.errorHandler)
		c.errorHandler.Release()
	}
}

func (c *Conn) LocalAddr() net.Addr {
	return c.localAddr
}

func getRemoteAddr(val js.Value) net.Addr {
	rawURL := val.Get("url").String()
	withoutPrefix := strings.TrimPrefix(rawURL, "ws://")
	withoutSuffix := strings.TrimSuffix(withoutPrefix, "/")
	return NewAddr(withoutSuffix)
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// TODO: Return os.ErrNoDeadline. For now we return nil because multiplexers
// don't handle the error correctly.
func (c *Conn) SetDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *Conn) setUpHandlers() {
	c.mut.Lock()
	defer c.mut.Unlock()
	if c.messageHandler != nil {
		// Message handlers already created. Nothing to do.
		return
	}
	messageHandler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		arrayBuffer := args[0].Get("data")
		data := arrayBufferToBytes(arrayBuffer)
		c.currDataMut.Lock()
		if _, err := c.currData.Write(data); err != nil {
			c.currDataMut.Unlock()
			return err
		}
		c.currDataMut.Unlock()

		// Non-blocking signal
		select {
		case c.dataSignal <- struct{}{}:
		default:
		}

		return nil
	})
	c.messageHandler = &messageHandler
	c.Call("addEventListener", "message", messageHandler)

	closeHandler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		go func() {
			c.signalClose()
			c.mut.Lock()
			// Store the error in c.firstErr. It will be returned by Read later on.
			c.firstErr = errorEventToError(args[0])
			c.mut.Unlock()
			c.releaseHandlers()
		}()
		return nil
	})
	c.closeHandler = &closeHandler
	c.Call("addEventListener", "close", closeHandler)

	errorHandler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		// Unfortunately, the "error" event doesn't appear to give us any useful
		// information. All we can do is close the connection.
		c.Close()
		return nil
	})
	c.errorHandler = &errorHandler
	c.Call("addEventListener", "error", errorHandler)
}

func (c *Conn) waitForOpen() error {
	openSignal := make(chan struct{})
	handler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		close(openSignal)
		return nil
	})
	defer c.Call("removeEventListener", "open", handler)
	defer handler.Release()
	c.Call("addEventListener", "open", handler)
	select {
	case <-openSignal:
		return nil
	case <-c.closeSignal:
		// c.closeSignal means there was an error when trying to open the
		// connection.
		return c.firstErr
	}
}

// arrayBufferToBytes converts a JavaScript ArrayBuffer to a slice of bytes.
func arrayBufferToBytes(buffer js.Value) []byte {
	view := js.Global().Get("Uint8Array").New(buffer)
	dataLen := view.Get("length").Int()
	data := make([]byte, dataLen)
	for i := 0; i < dataLen; i++ {
		data[i] = byte(view.Index(i).Int())
	}
	return data
}

func errorEventToError(val js.Value) error {
	var typ string
	if gotType := val.Get("type"); gotType != js.Undefined() {
		typ = gotType.String()
	} else {
		typ = val.Type().String()
	}
	var reason string
	if gotReason := val.Get("reason"); gotReason != js.Undefined() && gotReason.String() != "" {
		reason = gotReason.String()
	} else {
		code := val.Get("code")
		if code != js.Undefined() {
			switch code := code.Int(); code {
			case 1006:
				reason = "code 1006: connection unexpectedly closed"
			default:
				reason = fmt.Sprintf("unexpected code: %d", code)
			}
		}
	}
	return fmt.Errorf("JavaScript error: (%s) %s", typ, reason)
}
