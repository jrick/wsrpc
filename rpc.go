/*
Package wsrpc provides a partial implementation of a JSON-RPC 2.0 websocket
client.  Inspired by net/rpc, clients call methods by their name with arguments
and return values marshaled by encoding/json.  The client may be used to create
convenience calls with types specific to an application.

Receiving notifications is supported but it is up to the caller to unmarshal the
JSON-RPC parameters into meaningful data.

This package currently does not implement JSON-RPC 2.0 request batching or keyed
request parameters when performing calls.
*/
package wsrpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime/trace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const writeWait = 10 * time.Second // allowed duration before timing out a write

// Error represents a JSON-RPC error object.
type Error struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Notifier handles JSON-RPC notifications.  Method defines the type of
// notification and params describes the arguments (positional or keyed) if any
// were included in the Request object.
//
// Notify is never called concurrently and is called with notifications in the
// order received.  Blocking in Notify only blocks other Notify calls and does
// not prevent the Client from receiving further buffered notifications and
// processing calls.
//
// If Notify returns an error, the client is closed and no more notifications
// are processed.  If this is the first error observed by the client, it will be
// returned by Err.
//
// If Notifier implements io.Closer, Close is called following the final
// notification.
type Notifier interface {
	Notify(method string, params json.RawMessage) error
}

type call struct {
	method string
	result interface{}
	err    chan error
}

type request struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params,omitempty"`
	ID      uint32        `json:"id"`

	ctx context.Context
}

// Client implements JSON-RPC calls and notifications over a websocket.
type Client struct {
	atomicSeq  uint32
	addr       string
	ws         *websocket.Conn
	pongWait   time.Duration
	pingPeriod time.Duration
	notify     Notifier
	calls      map[uint32]*call
	callMu     sync.Mutex
	send       chan *request
	errMu      sync.Mutex    // synchronizes writes to err before errc is closed
	errc       chan struct{} // closed after err is set
	err        error
}

type options struct {
	tls        *tls.Config
	header     http.Header
	dial       DialFunc
	notify     Notifier
	pongWait   time.Duration
	pingPeriod time.Duration
}

// Option modifies the behavior of Dial.
type Option func(*options)

// DialFunc dials a network connection.  Custom dialers may utilize a proxy or
// set connection timeouts.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// WithDial specifies a custom dial function.
func WithDial(dial DialFunc) Option {
	return func(o *options) {
		o.dial = dial
	}
}

// WithBasicAuth enables basic access authentication using the user and
// password.
func WithBasicAuth(user, pass string) Option {
	return func(o *options) {
		if o.header == nil {
			o.header = make(http.Header)
		}
		o.header.Add("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
}

// WithTLSConfig specifies a TLS config when connecting to a secure websocket
// (wss) server.  If unspecified, the default TLS config will be used.
func WithTLSConfig(tls *tls.Config) Option {
	return func(o *options) {
		o.tls = tls
	}
}

// WithNotifier specifies a Notifier to handle received JSON-RPC notifications.
// Notifications may continue to be processed after the client has closed.
// Notifications are dropped by Client if a Notifier is not configured.
func WithNotifier(n Notifier) Option {
	return func(o *options) {
		o.notify = n
	}
}

// WithPingPeriod specifies a duration between pings sent on a timer.  A pong
// message not received within this period (plus a tolerance) causes connection
// termination.  A period of 0 disables the mechanism.
//
// The default value is one minute.
func WithPingPeriod(period time.Duration) Option {
	return func(o *options) {
		o.pingPeriod = period
		o.pongWait = 10 * period / 9
	}
}

// WithoutPongDeadline disables any default or custom pong deadline.
// Pings will still be written every ping period unless disabled.
// This option is reset by later WithPingPeriod options.
func WithoutPongDeadline() Option {
	return func(o *options) {
		o.pongWait = 0
	}
}

// Dial establishes an RPC client connection to the server described by addr.
// Addr must be the URL of the websocket, e.g., "wss://[::1]:9109/ws".
func Dial(ctx context.Context, addr string, opts ...Option) (*Client, error) {
	ctx, task := trace.NewTask(ctx, "Dial")
	defer task.End()
	var o options
	o.pingPeriod = 60 * time.Second
	o.pongWait = 10 * o.pingPeriod / 9
	for _, f := range opts {
		f(&o)
	}
	dialer := websocket.Dialer{
		NetDialContext:    o.dial,
		TLSClientConfig:   o.tls,
		EnableCompression: true,
	}
	ws, _, err := dialer.DialContext(ctx, addr, o.header)
	if err != nil {
		return nil, err
	}
	c := &Client{
		addr:       addr,
		ws:         ws,
		pongWait:   o.pongWait,
		pingPeriod: o.pingPeriod,
		notify:     o.notify,
		calls:      make(map[uint32]*call),
		send:       make(chan *request),
		errc:       make(chan struct{}),
	}
	var pingTicker *time.Ticker
	if o.pingPeriod != 0 {
		ws.SetPongHandler(func(string) error {
			defer trace.StartRegion(ctx, "PongHandler").End()
			trace.Logf(ctx, "", "received pong")
			if c.pongWait != 0 {
				readDeadline := time.Now().Add(c.pongWait)
				trace.Logf(ctx, "", "setting new read deadline %v", readDeadline)
				ws.SetReadDeadline(readDeadline)
			}
			return nil
		})
		// Initial read deadline must be set for the first ping message
		// sent pingPeriod from now.
		if c.pongWait != 0 {
			readDeadline := time.Now().Add(c.pingPeriod + c.pongWait)
			trace.Logf(ctx, "", "setting first read deadline %v", readDeadline)
			ws.SetReadDeadline(readDeadline)
		}
		pingTicker = time.NewTicker(c.pingPeriod)
	}
	go c.in(ctx)
	go c.out(ctx, pingTicker)
	return c, nil
}

// String returns the dialed websocket URL.
func (c *Client) String() string {
	return c.addr
}

// Close sends a websocket close control message and closes the underlying
// network connection.
func (c *Client) Close() error {
	// WriteControl and Close may be called concurrently with all other
	// websocket methods.
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	writeErr := c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(writeWait))
	closeErr := c.ws.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (c *Client) setErr(ctx context.Context, err error) {
	c.errMu.Lock()
	if c.err == nil {
		trace.Logf(ctx, "error", "%v", err)
		c.err = err
		close(c.errc)
		if closer, ok := c.notify.(io.Closer); ok {
			closer.Close()
		}
	}
	c.errMu.Unlock()
}

func (c *Client) out(ctx context.Context, pingTicker *time.Ticker) {
	ctx, task := trace.NewTask(ctx, "wsrpc.Client.out")
	defer task.End()

	defer c.ws.Close()

	var pingChan <-chan time.Time
	if pingTicker != nil {
		pingChan = pingTicker.C
		defer pingTicker.Stop()
	}

	for {
		// Give pings priority
		select {
		case <-pingChan:
			c.ping(ctx)
			continue
		default:
		}

		select {
		case <-c.Done():
			return
		case <-pingChan:
			c.ping(ctx)
			continue
		case request := <-c.send:
			writeDeadline := time.Now().Add(writeWait)
			trace.Logf(request.ctx, "", "setting write deadline %v", writeDeadline)
			c.ws.SetWriteDeadline(writeDeadline)
			err := c.ws.WriteJSON(request)
			trace.Logf(request.ctx, "", "wrote request")
			if err != nil {
				c.setErr(ctx, err)
				return
			}
		}
	}
}

func (c *Client) ping(ctx context.Context) {
	ctx, task := trace.NewTask(ctx, "ping")
	defer task.End()

	writeDeadline := time.Now().Add(writeWait)
	trace.Logf(ctx, "", "setting write deadline %v", writeDeadline)
	c.ws.SetWriteDeadline(writeDeadline)
	trace.Logf(ctx, "", "sending ping message")
	err := c.ws.WriteMessage(websocket.PingMessage, nil)
	if err != nil {
		trace.Logf(ctx, "", "writing ping failed: %v", err)
		c.setErr(ctx, err)
	}
}

func (c *Client) in(ctx context.Context) {
	ctx, task := trace.NewTask(ctx, "wsrpc.Client.in")
	defer task.End()
	tracelog := func(format string, args ...interface{}) {
		trace.Logf(ctx, "in", format, args...)
	}

	// pair of channel vars retains notification processing order
	block, unblockNext := make(chan struct{}), make(chan struct{})
	close(block)

	for {
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
			ID     uint32          `json:"id"`

			// Request fields for notifications
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		tracelog("reading websocket")
		err := c.ws.ReadJSON(&resp)
		if err != nil {
			tracelog("websocket read failed: %v", err)
			c.setErr(ctx, err)
			return
		}
		tracelog("finished websocket read")

		// Zero IDs are never used by requests
		if resp.Method != "" && resp.Result == nil && resp.Error == nil && resp.ID == 0 {
			// it's a notification
			if c.notify != nil {
				tracelog("queueing notifier for method %q", resp.Method)
				go func(block, unblockNext chan struct{}) {
					select {
					case <-c.errc:
						return
					case <-block:
					}
					tracelog("running notifier for method %q", resp.Method)
					err := c.notify.Notify(resp.Method, resp.Params)
					if err != nil {
						c.setErr(ctx, err)
						c.ws.Close()
					}
					close(unblockNext)
				}(block, unblockNext)
				block, unblockNext = unblockNext, make(chan struct{})
			}
			continue
		}
		c.callMu.Lock()
		call, ok := c.calls[resp.ID]
		c.callMu.Unlock()
		if !ok {
			tracelog("unknown response ID")
			c.setErr(ctx, errors.New("wsrpc: unknown response ID"))
			return
		}
		if resp.Error != nil {
			err = resp.Error
		} else if call.result != nil {
			err = json.NewDecoder(bytes.NewReader(resp.Result)).Decode(call.result)
		}
		call.err <- err
	}
}

// Call performs the JSON-RPC described by method with positional parameters
// passed through args.  Result should point to an object to unmarshal the
// result, or equal nil to discard the result.
func (c *Client) Call(ctx context.Context, method string, result interface{}, args ...interface{}) (err error) {
	ctx, task := trace.NewTask(ctx, method)
	defer task.End()

	defer func() {
		if e := ctx.Err(); e != nil {
			err = e
		}
	}()

	id := atomic.AddUint32(&c.atomicSeq, 1)
	if id == 0 {
		// Zero IDs are reserved to indicate missing ID fields in notifications
		id = atomic.AddUint32(&c.atomicSeq, 1)
	}
	call := &call{
		method: method,
		result: result,
		err:    make(chan error, 1),
	}
	c.callMu.Lock()
	c.calls[id] = call
	c.callMu.Unlock()

	req := &request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  args,
		ID:      id,
		ctx:     ctx,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.errc:
		return c.err
	case c.send <- req:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.errc:
		return c.err
	case err := <-call.err:
		return err
	}
}

// Done returns a channel that is closed after the client's final error is set.
func (c *Client) Done() <-chan struct{} {
	return c.errc
}

// Err blocks until the client has shutdown and returns the final error.
func (c *Client) Err() error {
	<-c.errc
	return c.err
}
