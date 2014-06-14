package enproxy

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	X_HTTPCONN_ID         = "X-HTTPConn-Id"
	X_HTTPCONN_DEST_ADDR  = "X-HTTPConn-Dest-Addr"
	X_HTTPCONN_EOF        = "X-HTTPConn-EOF"
	X_HTTPCONN_PROXY_HOST = "X-HTTPConn-Proxy-Host"
)

var (
	defaultIdleInterval = 15 * time.Millisecond
	defaultIdleTimeout  = 70 * time.Second

	emptyBuffer = []byte{}
)

// Conn is a net.Conn that tunnels its data via an httpconn.Proxy using HTTP
// requests and responses.  It assumes that streaming requests are not supported
// by the underlying servers/proxies, and so uses a polling technique similar to
// the one used by meek, but different in that data is not encoded as JSON.
// https://trac.torproject.org/projects/tor/wiki/doc/AChildsGardenOfPluggableTransports#Undertheencryption.
//
// enproxy uses two parallel channels to send and receive data.  One channel
// handles writing data out by making sequential POST requests to the server
// which encapsulate the outbound data in their request bodies, while the other
// channel handles reading data by making GET requests and grabbing the data
// encapsulated in the response bodies.
//
// Write Channel:
//
//   1. Accept writes, piping these to the proxy as the body of an http POST
//   2. Continue to pipe the writes until the pause between consecutive writes
//      exceeds the IdleInterval, at which point we finish the request body. We
//      do this because it is assumed that intervening proxies (e.g. CloudFlare
//      CDN) do not allow streaming requests, so it is necessary to finish the
//      request for data to get flushed to the destination server.
//   3. After receiving a response to the POST request, return to step 1
//
// Read Channel:
//
//   1. Accept reads, issuing a new GET request if one is not already ongoing
//   2. Process read by grabbing data from the response to the GET request
//   3. Continue to accept reads, grabbing these from the response of the
//      existing GET request
//   4. Once the response to the GET request reaches EOF, return to step 1. This
//      will happen because the proxy periodically closes responses to make sure
//      intervening proxies don't time out.
//   5. If a response is received with a special header indicating a true EOF
//      from the destination server, return EOF to the reader
//
type Conn struct {
	// Addr: the host:port of the destination server that we're trying to reach
	Addr string

	// Config: configuration of this Conn
	Config *Config

	// proxyHostCh: Self-reported FQDN of the proxy serving this connection.
	// This allows us to guarantee we reach the same server in subsequent
	// requests, even if it was initially reached through a FQDN that may
	// resolve to different IPs in different DNS lookups (e.g. as in DNS round
	// robin).
	proxyHostCh chan string

	// id: unique identifier for this connection
	id string

	/* Channels for processing reads, writes and closes */
	writeRequestsCh  chan []byte     // requests to write
	writeResponsesCh chan rwResponse // responses for writes
	stopWriteCh      chan interface{}
	doneWriting      bool
	writeMutex       sync.RWMutex
	readRequestsCh   chan []byte     // requests to read
	readResponsesCh  chan rwResponse // responses for reads
	stopReadCh       chan interface{}
	doneReading      bool
	readMutex        sync.RWMutex
	reqOutCh         chan *io.PipeReader // channel for next outgoing request body
	stopReqCh        chan interface{}
	doneRequesting   bool
	requestMutex     sync.RWMutex

	/* Fields for tracking activity/closed status */
	lastActivityTime  time.Time    // time of last read or write
	lastActivityMutex sync.RWMutex // mutex controlling access to lastActivityTime
	closed            bool         // whether or not this Conn is closed
	closedMutex       sync.RWMutex // mutex controlling access to closed flag

	/* Fields for tracking current request and response */
	reqBodyWriter *io.PipeWriter // pipe writer to current request body
	resp          *http.Response // the current response being used to read data
}

type dialFunc func(addr string) (net.Conn, error)

type newRequestFunc func(host string, method string, body io.Reader) (*http.Request, error)

// rwResponse is a response to a read or write
type rwResponse struct {
	n   int
	err error
}

// Config configures a Conn
type Config struct {
	// DialProxy: function to open a connection to the proxy
	DialProxy dialFunc

	// NewRequest: function to create a new request to the proxy
	NewRequest newRequestFunc

	// IdleInterval: how long to let the write idle before writing out a
	// request to the proxy.  Defaults to 15 milliseconds.
	IdleInterval time.Duration

	// IdleTimeout: how long to wait before closing an idle connection, defaults
	// to 70 seconds.  The high default value is selected to work well with XMPP
	// traffic tunneled over enproxy by Lantern.
	IdleTimeout time.Duration
}

// LocalAddr() is not implemented
func (c *Conn) LocalAddr() net.Addr {
	panic("LocalAddr() not implemented")
}

// RemoteAddr() is not implemented
func (c *Conn) RemoteAddr() net.Addr {
	panic("RemoteAddr() not implemented")
}

// Write() implements the function from net.Conn
func (c *Conn) Write(b []byte) (n int, err error) {
	if c.submitWrite(b) {
		res, ok := <-c.writeResponsesCh
		if !ok {
			return 0, io.EOF
		} else {
			return res.n, res.err
		}
	} else {
		return 0, io.EOF
	}
}

// Read() implements the function from net.Conn
func (c *Conn) Read(b []byte) (n int, err error) {
	if c.submitRead(b) {
		res, ok := <-c.readResponsesCh
		if !ok {
			return 0, io.EOF
		} else {
			return res.n, res.err
		}
	} else {
		return 0, io.EOF
	}
}

// Close() implements the function from net.Conn
func (c *Conn) Close() error {
	c.closedMutex.Lock()
	defer c.closedMutex.Unlock()
	if !c.closed {
		c.stopReadCh <- nil
		c.stopWriteCh <- nil
		c.stopReqCh <- nil
		c.closed = true
	}
	return nil
}

func (c *Conn) isClosed() bool {
	c.closedMutex.RLock()
	defer c.closedMutex.RUnlock()
	return c.closed
}

// SetDeadline() is currently unimplemented.
func (c *Conn) SetDeadline(t time.Time) error {
	panic("SetDeadline not implemented")
}

// SetReadDeadline() is currently unimplemented.
func (c *Conn) SetReadDeadline(t time.Time) error {
	panic("SetReadDeadline not implemented")
}

// SetWriteDeadline() is currently unimplemented.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	panic("SetWriteDeadline not implemented")
}
