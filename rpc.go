// Copyright (c) 2019 Josh Rickmar
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

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
package wsrpc // import "github.com/jrick/wsrpc"

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// Error represents a JSON-RPC error object.
type Error struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Notification represents a JSON-RPC notification.  Method defines the type of
// notification and Params describes the arguments (positional or keyed) if any
// were included in the Request object.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Notifier is a channel of received JSON-RPC notifications.  Notifications are
// sent in the order received.  Notifier is closed after the client connection
// is broken.  Notifications may be read from the channel after client
// disconnect if there is enough backpressure.
type Notifier chan Notification

type call struct {
	method string
	result interface{}
	err    chan error
}

// Client implements JSON-RPC calls and notifications over a websocket.
type Client struct {
	atomicSeq uint32
	addr      string
	ws        *websocket.Conn
	notify    Notifier
	calls     map[uint32]*call
	callMu    sync.Mutex
	writing   sync.Mutex
	errc      chan struct{} // closed after err is set
	err       error
}

type options struct {
	tls    *tls.Config
	header http.Header
	dial   DialFunc
	notify Notifier
}

// Option modifies the behavior of Dial.
type Option func(*options)

// DialFunc dials a network connection.  Custom dialers may utilize a proxy or
// set connection timeouts.
type DialFunc func(network, address string) (net.Conn, error)

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

// WithNotifier specifies a channel to read received JSON-RPC notifications.
// The channel is closed when no futher notifications will be sent.
// Notifications may be read from the channel after the client has closed.
func WithNotifier(n Notifier) Option {
	return func(o *options) {
		o.notify = n
	}
}

// Dial establishes an RPC client connection to the server described by addr.
// Addr must be the URL of the websocket, e.g., "wss://[::1]:9109/ws".
func Dial(ctx context.Context, addr string, opts ...Option) (*Client, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	dialer := websocket.Dialer{
		NetDial:           o.dial,
		TLSClientConfig:   o.tls,
		EnableCompression: true,
	}
	ws, _, err := dialer.DialContext(ctx, addr, o.header)
	if err != nil {
		return nil, err
	}
	c := &Client{
		addr:   addr,
		ws:     ws,
		notify: o.notify,
		calls:  make(map[uint32]*call),
		errc:   make(chan struct{}),
	}
	go c.in()
	return c, nil
}

// Address returns the dialed network address.
func (c *Client) Address() string {
	return c.addr
}

// Close closes the underlying websocket connection.
func (c *Client) Close() error {
	return c.ws.Close()
}

func (c *Client) in() {
	// pair of channel vars retains notification processing order
	block, unblockNext := make(chan struct{}), make(chan struct{})
	close(block)
	// notify chan is closed in background after final notification is sent
	if c.notify != nil {
		defer func() {
			go func() {
				<-block
				close(c.notify)
			}()
		}()
	}
	for {
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
			ID     uint32          `json:"id"`

			// Request fields for notifications
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		err := c.ws.ReadJSON(&resp)
		if err != nil {
			c.err = err
			close(c.errc)
			return
		}
		// Zero IDs are never used by requests
		if resp.Method != "" && resp.Result == nil && resp.Error == nil && resp.ID == 0 {
			// it's a notification
			if c.notify != nil {
				go func(block, unblockNext chan struct{}) {
					<-block
					c.notify <- Notification{resp.Method, resp.Params}
					unblockNext <- struct{}{}
				}(block, unblockNext)
				block, unblockNext = unblockNext, make(chan struct{})
			}
			continue
		}
		c.callMu.Lock()
		call, ok := c.calls[resp.ID]
		c.callMu.Unlock()
		if !ok {
			c.err = errors.New("wsrpc: unknown response ID")
			close(c.errc)
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

	request := &struct {
		JSONRPC string        `json:"jsonrpc"`
		Method  string        `json:"method"`
		Params  []interface{} `json:"params,omitempty"`
		ID      uint32        `json:"id"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  args,
		ID:      id,
	}
	c.writing.Lock()
	err = c.ws.WriteJSON(request)
	c.writing.Unlock()
	if err != nil {
		return err
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

// Err blocks until the client has shutdown and returns the final error.
func (c *Client) Err() error {
	<-c.errc
	return c.err
}
