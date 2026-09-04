package realtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Timeouts for a control-plane socket. They are constants rather than
// configuration because both ends of the contract have to agree on them and
// there is no deployment where a different value is right.
const (
	// writeTimeout bounds one frame write. A daemon on a slow uplink still
	// writes a frame in well under this; anything longer is a dead peer whose
	// TCP stack has not noticed yet.
	writeTimeout = 15 * time.Second
	// pingInterval is how often the server probes a quiet connection. It is
	// what turns "the laptop was closed" into a closed socket rather than a
	// connection that looks online forever.
	pingInterval = 20 * time.Second
	// pingTimeout is how long a probe may go unanswered.
	pingTimeout = 10 * time.Second
)

// MaxFrameBytes bounds one inbound frame. A run.result carries up to 500
// execution results and 2000 artifact records, which is large but bounded by
// the contract; anything past this is not a frame we agreed to accept.
const MaxFrameBytes int64 = 16 << 20 // 16 MiB

// conn is one WebSocket with a serialised write side.
//
// coder/websocket permits exactly one concurrent writer, and this connection
// has several: the read loop's replies, the scheduler's run.assign, a cancel
// from an HTTP handler, and the keepalive. The mutex is what makes those safe
// rather than an intermittent "concurrent write" failure under load.
type conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex

	closeOnce sync.Once
	// closed is closed when this connection is finished, so a send racing a
	// shutdown fails fast instead of blocking on a dead socket.
	closed chan struct{}
}

func newConn(ws *websocket.Conn) *conn {
	ws.SetReadLimit(MaxFrameBytes)
	return &conn{ws: ws, closed: make(chan struct{})}
}

// send writes one text frame.
func (c *conn) send(ctx context.Context, frame []byte) error {
	select {
	case <-c.closed:
		return ErrRuntimeOffline
	default:
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, frame); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// closeWith ends the connection with a status the other end can act on.
// Calling it twice is a no-op, which is what lets both a handler's defer and
// an error path call it.
func (c *conn) closeWith(status websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		close(c.closed)
		// A WebSocket close frame allows 123 bytes of reason.
		if len(reason) > 120 {
			reason = reason[:120]
		}
		_ = c.ws.Close(status, reason)
	})
}

// close ends a connection the peer got wrong.
func (c *conn) close(reason string) { c.closeWith(websocket.StatusPolicyViolation, reason) }

// closeNormal ends a connection that finished for an ordinary reason.
func (c *conn) closeNormal(reason string) { c.closeWith(websocket.StatusNormalClosure, reason) }

// keepalive pings until ctx ends or the peer stops answering. A returned error
// is always "this connection is gone".
func (c *conn) keepalive(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				c.close("no response to keepalive")
				return
			}
		}
	}
}
