package client

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	initialConnectionReadBufSize  = 16 * 1024
	initialConnectionWriteBufSize = 16 * 1024
)

var (
	connectionReadBufPool = sync.Pool{
		New: func() interface{} { return make([]byte, initialConnectionReadBufSize) },
	}
	connectionWriteBufPool = sync.Pool{
		New: func() interface{} { return make([]byte, initialConnectionWriteBufSize) },
	}

	// errRespTransferred is a package-internal sentinel returned by doPipelined
	// when the caller's context fires. It tells DoWithContext that the resp
	// object's ownership has been transferred to the ResponseReader goroutine,
	// so DoWithContext must NOT release it.
	errRespTransferred = errors.New("internal: resp ownership transferred to pipeline reader")
)

// PendingRequest represents a request queued for a pipelined response.
// It is added to pendingReqs only AFTER the request has been written to the wire.
type PendingRequest struct {
	req       *Request
	response  chan *Response
	err       chan error
	cancelled atomic.Bool // true when caller's context fired
	// releaseFn is called by the ResponseReader after it finishes processing a
	// cancelled request's response. This prevents returning resp to the pool
	// while the ResponseReader still holds a reference to it.
	releaseFn func(*Response)
}

// Connection owns a single TCP/TLS connection with optional pipelining.
type Connection struct {
	id          int
	host        string // bare hostname
	port        int    // TCP port
	config      *Config
	dialer      *Dialer
	tlsConfig   *TLSConfig
	compressor  *Compressor
	conn        atomic.Value // stores *net.Conn
	stopped     atomic.Int32
	healthy     atomic.Int32
	cachedNow   atomic.Int64
	timeCounter atomic.Uint32
	reqsOnConn  atomic.Int32
	lastUsed    atomic.Int64
	activeReqs  atomic.Int32

	// Connection establishment — serializes concurrent reconnection attempts.
	connectMu sync.Mutex

	// Pipelining — all access serialized by pendingMu.
	pendingReqs    []*PendingRequest
	pendingMu      sync.Mutex
	responseReader *ResponseReader
	readerStopped  atomic.Int32
	closing        atomic.Int32
	pendingSignal  chan struct{}

	// Hot-path cached config.
	readBufSize    int
	writeBufSize   int
	maxRespSize    int
	maxReqSize     int
	headerReadSize int
	bodyReadSize   int

	// Forward-proxy mode: connect to proxy TCP, send absolute-URI requests.
	isForwardProxy  bool
	proxyAuthHeader []byte
}

// ResponseReader is a per-connection goroutine that reads responses in FIFO order.
// It uses a persistent bufio.Reader to preserve any leftover bytes that arrived
// in the same TCP segment as a previous response (essential for pipelining).
type ResponseReader struct {
	conn     *Connection
	br       *bufio.Reader // wraps the TCP conn; preserves inter-response leftover
	scratch  []byte        // scratch buffer for header parsing
	stopChan chan struct{}
}

// NewConnection creates a Connection. TCP is not established until first use.
func NewConnection(id int, host string, port int, config *Config, dialer *Dialer, tlsConfig *TLSConfig, compressor *Compressor) *Connection {
	now := time.Now().UnixNano()

	readBufSize := config.ReadBufferSize
	if readBufSize <= 0 {
		readBufSize = 64 * 1024
	}
	writeBufSize := config.WriteBufferSize
	if writeBufSize <= 0 {
		writeBufSize = 64 * 1024
	}
	maxRespSize := config.MaxResponseBodySize
	if maxRespSize <= 0 {
		maxRespSize = 10 * 1024 * 1024
	}
	maxReqSize := config.MaxRequestBodySize
	if maxReqSize <= 0 {
		maxReqSize = 10 * 1024 * 1024
	}
	headerReadSize := config.HeaderReadChunkSize
	if headerReadSize <= 0 {
		headerReadSize = 4 * 1024
	}
	bodyReadSize := config.BodyReadChunkSize
	if bodyReadSize <= 0 {
		bodyReadSize = 64 * 1024
	}

	useTLS := tlsConfig != nil
	isForwardProxy := dialer != nil && dialer.IsForwardProxy(useTLS)
	var proxyAuthHeader []byte
	if isForwardProxy {
		proxyAuthHeader = dialer.ProxyAuthHeader()
	}

	c := &Connection{
		id:              id,
		host:            host,
		port:            port,
		config:          config,
		dialer:          dialer,
		tlsConfig:       tlsConfig,
		compressor:      compressor,
		pendingReqs:     make([]*PendingRequest, 0, 16),
		pendingSignal:   make(chan struct{}, 1),
		readBufSize:     readBufSize,
		writeBufSize:    writeBufSize,
		maxRespSize:     maxRespSize,
		maxReqSize:      maxReqSize,
		headerReadSize:  headerReadSize,
		bodyReadSize:    bodyReadSize,
		isForwardProxy:  isForwardProxy,
		proxyAuthHeader: proxyAuthHeader,
	}
	c.cachedNow.Store(now)
	c.lastUsed.Store(now)
	return c
}

func (c *Connection) getReadBuf(requiredSize int) []byte {
	buf := connectionReadBufPool.Get().([]byte)
	if len(buf) >= requiredSize {
		return buf
	}
	newSize := len(buf) * 2
	if newSize < requiredSize {
		newSize = requiredSize
	}
	if newSize > c.readBufSize {
		newSize = c.readBufSize
		if newSize < requiredSize {
			connectionReadBufPool.Put(buf)
			return make([]byte, requiredSize)
		}
	}
	if newSize%4096 != 0 {
		newSize = ((newSize / 4096) + 1) * 4096
	}
	connectionReadBufPool.Put(buf)
	return make([]byte, newSize)
}

func (c *Connection) putReadBuf(buf []byte) {
	if buf == nil {
		return
	}
	if len(buf) > initialConnectionReadBufSize*4 {
		buf = make([]byte, initialConnectionReadBufSize)
	}
	connectionReadBufPool.Put(buf)
}

func (c *Connection) getWriteBuf(requiredSize int) []byte {
	buf := connectionWriteBufPool.Get().([]byte)
	if len(buf) >= requiredSize {
		return buf
	}
	newSize := len(buf) * 2
	if newSize < requiredSize {
		newSize = requiredSize
	}
	if newSize > c.writeBufSize {
		newSize = c.writeBufSize
		if newSize < requiredSize {
			connectionWriteBufPool.Put(buf)
			return make([]byte, requiredSize)
		}
	}
	if newSize%4096 != 0 {
		newSize = ((newSize / 4096) + 1) * 4096
	}
	connectionWriteBufPool.Put(buf)
	return make([]byte, newSize)
}

func (c *Connection) putWriteBuf(buf []byte) {
	if buf == nil {
		return
	}
	if len(buf) > initialConnectionWriteBufSize*4 {
		buf = make([]byte, initialConnectionWriteBufSize)
	}
	connectionWriteBufPool.Put(buf)
}

// IsHealthy returns true if the connection is alive and not stopped.
func (c *Connection) IsHealthy() bool {
	return c.healthy.Load() == 1 && c.stopped.Load() == 0
}

// CanAcceptRequest returns true when the connection can take another request.
func (c *Connection) CanAcceptRequest() bool {
	if !c.IsHealthy() {
		return false
	}
	if c.config.EnablePipelining {
		max := c.config.MaxPipelinedRequests
		if max <= 0 {
			max = 10
		}
		return int(c.activeReqs.Load()) < max
	}
	return c.activeReqs.Load() == 0
}

// Do executes a request on this connection.
func (c *Connection) Do(req *Request) (*Response, error) {
	if req == nil {
		return nil, WrapError(ErrorTypeValidation, "request is nil", ErrInvalidResponse)
	}
	if !c.ensureConnection() {
		return nil, WrapError(ErrorTypeNetwork, "connection failed", ErrConnectFailed)
	}
	if c.config.EnablePipelining {
		return c.doPipelined(req)
	}
	return c.doSequential(req)
}

func (c *Connection) doSequential(req *Request) (*Response, error) {
	c.activeReqs.Add(1)
	defer c.activeReqs.Add(-1)

	if ctxDone := ctxDoneChan(req.ctx); ctxDone != nil {
		select {
		case <-ctxDone:
			return nil, WrapError(ErrorTypeTimeout, "request cancelled", req.ctx.Err())
		default:
		}
	}

	if c.config.MaxRequestsPerConn > 0 && int(c.reqsOnConn.Load()) >= c.config.MaxRequestsPerConn {
		c.closeConn()
		if !c.ensureConnection() {
			return nil, WrapError(ErrorTypeNetwork, "connection not available", ErrConnectFailed)
		}
	}

	connVal := c.conn.Load()
	if connVal == nil {
		return nil, WrapError(ErrorTypeNetwork, "connection is nil", ErrConnectFailed)
	}
	connPtr := connVal.(*net.Conn)
	if connPtr == nil || *connPtr == nil {
		return nil, WrapError(ErrorTypeNetwork, "connection is nil", ErrConnectFailed)
	}
	conn := *connPtr

	body, compressed, err := c.prepareBody(req)
	if err != nil {
		return nil, err
	}

	writeTimeout := req.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = c.config.WriteTimeout
	}
	readTimeout := c.effectiveReadTimeout(req)

	resp := req.resp
	if resp == nil {
		return nil, WrapError(ErrorTypeInternal, "response object is nil", ErrInvalidResponse)
	}
	resp.Reset()

	if req.ExpectContinue && len(body) > 0 {
		if err := c.writeRequest(conn, req, nil, compressed, writeTimeout); err != nil {
			c.healthy.Store(0)
			return nil, err
		}
		continueResp := &Response{bodyBuf: make([]byte, 512), Headers: make([]Header, 0, 4)}
		if err := c.readResponse(conn, continueResp, readTimeout, req.Method); err != nil {
			c.healthy.Store(0)
			return nil, err
		}
		if continueResp.StatusCode != 100 {
			resp.StatusCode = continueResp.StatusCode
			resp.Headers = continueResp.Headers
			resp.Body = continueResp.Body
			resp.ContentLength = continueResp.ContentLength
			c.clearDeadlines(conn)
			c.reqsOnConn.Add(1)
			c.updateTimeCache()
			return resp, nil
		}
		if err := writeAll(conn, body); err != nil {
			c.healthy.Store(0)
			return nil, WrapError(ErrorTypeNetwork, "write body after 100-continue failed", err)
		}
	} else {
		if err := c.writeRequest(conn, req, body, compressed, writeTimeout); err != nil {
			c.healthy.Store(0)
			return nil, err
		}
	}

	if err := c.readResponse(conn, resp, readTimeout, req.Method); err != nil {
		if resp.StatusCode > 0 {
			c.clearDeadlines(conn)
			c.reqsOnConn.Add(1)
			c.updateTimeCache()
			return resp, nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			c.healthy.Store(0)
			return nil, WrapError(ErrorTypeTimeout, "read timeout", err)
		}
		if err == io.EOF {
			c.healthy.Store(0)
			return nil, WrapError(ErrorTypeNetwork, "connection closed by server", err)
		}
		c.healthy.Store(0)
		return nil, err
	}

	c.clearDeadlines(conn)
	c.reqsOnConn.Add(1)
	c.updateTimeCache()

	if resp.isConnectionClose() {
		c.healthy.Store(0)
	}

	return resp, nil
}

// doPipelined executes a request using HTTP/1.1 pipelining.
//
// Pipeline invariant: responses arrive in the same order as requests on the wire.
// To uphold this, a request is added to pendingReqs ONLY AFTER it has been
// successfully written to the wire. The response reader goroutine then reads
// responses in FIFO order and dispatches them to the correct caller.
//
// Cancellation: marking a pending as cancelled does NOT remove it from the
// queue. The reader still drains that response from the wire (keeping the
// pipeline in sync), but discards the data instead of delivering it.
func (c *Connection) doPipelined(req *Request) (*Response, error) {
	if c.closing.Load() != 0 {
		return nil, WrapError(ErrorTypeNetwork, "connection is closing", ErrConnectFailed)
	}

	// Connection rotation (before write).
	if c.config.MaxRequestsPerConn > 0 && int(c.reqsOnConn.Load()) >= c.config.MaxRequestsPerConn {
		c.closeConn()
		if !c.ensureConnection() {
			return nil, WrapError(ErrorTypeNetwork, "connection not available after rotation", ErrConnectFailed)
		}
	}

	// Prepare body before taking the write lock (CPU work, no I/O).
	body, compressed, err := c.prepareBody(req)
	if err != nil {
		return nil, err
	}

	writeTimeout := req.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = c.config.WriteTimeout
	}

	// Fast context check before the expensive write lock.
	if ctxDone := ctxDoneChan(req.ctx); ctxDone != nil {
		select {
		case <-ctxDone:
			return nil, WrapError(ErrorTypeTimeout, "request cancelled before write", req.ctx.Err())
		default:
		}
	}

	connVal := c.conn.Load()
	if connVal == nil {
		return nil, WrapError(ErrorTypeNetwork, "connection is nil", ErrConnectFailed)
	}
	connPtr := connVal.(*net.Conn)
	if connPtr == nil || *connPtr == nil {
		return nil, WrapError(ErrorTypeNetwork, "connection is nil", ErrConnectFailed)
	}
	conn := *connPtr

	pending := &PendingRequest{
		req:      req,
		response: make(chan *Response, 1),
		err:      make(chan error, 1),
	}

	//
	// This is the key invariant: every entry in pendingReqs has already been
	// written to the wire. The response reader can safely read responses in
	// pendingReqs order without worrying about requests not yet sent.
	c.pendingMu.Lock()
	if c.closing.Load() != 0 {
		c.pendingMu.Unlock()
		return nil, WrapError(ErrorTypeNetwork, "connection is closing", ErrConnectFailed)
	}

	// Ensure response reader is running.
	if c.responseReader == nil && c.readerStopped.Load() == 0 {
		c.startResponseReader()
	}

	c.activeReqs.Add(1)
	writeErr := c.writeRequest(conn, req, body, compressed, writeTimeout)
	if writeErr != nil {
		c.activeReqs.Add(-1)
		c.pendingMu.Unlock()
		c.closeConn()
		return nil, writeErr
	}

	// Only add to pending AFTER successful write.
	c.pendingReqs = append(c.pendingReqs, pending)
	c.pendingMu.Unlock()

	// Notify response reader (non-blocking; reader drains the full queue).
	select {
	case c.pendingSignal <- struct{}{}:
	default:
	}

	// Wait for response.
	ctxDone := ctxDoneChan(req.ctx)
	select {
	case resp := <-pending.response:
		return resp, nil
	case err := <-pending.err:
		return nil, err
	case <-ctxDone:
		// Transfer resp ownership to the ResponseReader so it can release it
		// after draining the response from the wire. The caller (DoWithContext)
		// must NOT touch resp after this point.
		pending.releaseFn = req.releaseFn
		pending.cancelled.Store(true)
		return nil, errRespTransferred
	}
}

// prepareBody compresses body if configured and validates size.
func (c *Connection) prepareBody(req *Request) (body []byte, compressed bool, err error) {
	body = req.Body
	if len(body) > c.maxReqSize {
		return nil, false, ErrRequestTooLarge
	}
	if req.Compressed && c.compressor != nil && len(body) > 0 {
		needed := len(body) + (len(body) / 10) + 100
		if req.maxBodySize > 0 && needed > req.maxBodySize {
			needed = req.maxBodySize
		}
		if !req.ensureBodyBufferSize(needed) {
			return nil, false, WrapError(ErrorTypeInternal, "body buffer too small for compression", ErrRequestTooLarge)
		}
		comp, compErr := c.compressor.CompressInto(req.bodyBuf, body)
		if compErr != nil {
			return nil, false, WrapError(ErrorTypeInternal, "compression failed", compErr)
		}
		if len(comp) > 0 {
			return comp, true, nil
		}
	}
	return body, false, nil
}

// effectiveReadTimeout returns the read timeout for req, capped by any context deadline.
func (c *Connection) effectiveReadTimeout(req *Request) time.Duration {
	t := req.ReadTimeout
	if t == 0 {
		t = c.config.ReadTimeout
	}
	if req.ctx != nil {
		if deadline, ok := req.ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 1 * time.Millisecond // will immediately timeout
			}
			if t == 0 || remaining < t {
				t = remaining
			}
		}
	}
	return t
}

// startResponseReader starts the reader goroutine. Must be called with pendingMu held.
func (c *Connection) startResponseReader() {
	if c.readerStopped.Load() != 0 || c.closing.Load() != 0 || c.responseReader != nil {
		return
	}
	connVal := c.conn.Load()
	if connVal == nil {
		return
	}
	connPtr := connVal.(*net.Conn)
	if connPtr == nil || *connPtr == nil {
		return
	}
	c.responseReader = &ResponseReader{
		conn:     c,
		br:       bufio.NewReaderSize(*connPtr, c.readBufSize),
		scratch:  make([]byte, c.readBufSize),
		stopChan: make(chan struct{}),
	}
	go c.responseReader.run()
}

// removePending removes pending from the queue (called with pendingMu held or separately).
func (c *Connection) removePending(pending *PendingRequest) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for i, p := range c.pendingReqs {
		if p == pending {
			c.pendingReqs = append(c.pendingReqs[:i], c.pendingReqs[i+1:]...)
			break
		}
	}
}

// ensureConnection establishes a TCP/TLS connection if not already connected.
// connectMu serializes concurrent reconnection attempts (double-checked locking).
func (c *Connection) ensureConnection() bool {
	if c.healthy.Load() == 1 {
		if c.config.IdleTimeout > 0 {
			now := c.cachedNow.Load()
			lastUsed := c.lastUsed.Load()
			if time.Duration(now-lastUsed) <= c.config.IdleTimeout {
				return true
			}
			// Idle timeout expired — fall through to reconnect.
		} else {
			return true
		}
	}

	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	if c.healthy.Load() == 1 {
		if c.config.IdleTimeout > 0 {
			now := c.cachedNow.Load()
			lastUsed := c.lastUsed.Load()
			if time.Duration(now-lastUsed) <= c.config.IdleTimeout {
				return true
			}
		} else {
			return true
		}
	}

	connVal := c.conn.Load()
	if connVal != nil {
		if connPtr, ok := connVal.(*net.Conn); ok && connPtr != nil {
			_ = (*connPtr).Close()
		}
		var nilConn *net.Conn
		c.conn.Store(nilConn)
	}
	c.healthy.Store(0)

	if err := c.connectLocked(); err != nil {
		return false
	}

	c.closing.Store(0)
	c.readerStopped.Store(0)

	now := time.Now().UnixNano()
	c.cachedNow.Store(now)
	c.lastUsed.Store(now)
	c.timeCounter.Store(0)
	return true
}

// connectLocked establishes a TCP (and optionally TLS) connection.
// A single attempt is made — the pool layer in GetConnection handles retries
// at a higher level, so retrying here would multiply timeouts unnecessarily
// (e.g. a 30s proxy CONNECT timeout would become 60s with two attempts).
func (c *Connection) connectLocked() error {
	if c.dialer == nil {
		return WrapError(ErrorTypeNetwork, "dialer is nil", ErrConnectFailed)
	}
	var conn net.Conn
	var err error
	if c.isForwardProxy {
		conn, err = c.dialer.DialForward()
	} else {
		conn, err = c.dialer.DialAddr(c.host, c.port)
	}
	if err != nil {
		return err
	}
	if c.tlsConfig != nil {
		tlsConn, tlsErr := c.tlsConfig.WrapConnection(conn)
		if tlsErr != nil {
			conn.Close()
			return tlsErr
		}
		conn = tlsConn
	}
	connPtr := &conn
	c.conn.Store(connPtr)
	c.reqsOnConn.Store(0)
	c.healthy.Store(1)
	return nil
}

// closeConn closes the underlying TCP connection and notifies all pending callers.
func (c *Connection) closeConn() {
	if !c.closing.CompareAndSwap(0, 1) {
		return
	}

	connVal := c.conn.Load()
	if connVal != nil {
		if connPtr, ok := connVal.(*net.Conn); ok && connPtr != nil {
			(*connPtr).Close()
		}
		var nilConn *net.Conn
		c.conn.Store(nilConn)
	}
	c.healthy.Store(0)

	closeErr := WrapError(ErrorTypeNetwork, "connection closed", ErrConnectFailed)
	c.pendingMu.Lock()
	for _, p := range c.pendingReqs {
		select {
		case p.err <- closeErr:
		default:
		}
	}
	c.pendingReqs = c.pendingReqs[:0]

	// Stop the ResponseReader under the same lock that starts it, eliminating
	// the data race between closeConn and startResponseReader.
	rr := c.responseReader
	c.responseReader = nil
	c.readerStopped.Store(1)
	c.pendingMu.Unlock()

	if rr != nil {
		select {
		case <-rr.stopChan:
		default:
			close(rr.stopChan)
		}
	}
}

// Stop permanently stops the connection.
func (c *Connection) Stop() {
	if !c.stopped.CompareAndSwap(0, 1) {
		return
	}
	c.closeConn()
}

// writeRequest serialises and sends the HTTP request to conn.
func (c *Connection) writeRequest(conn net.Conn, req *Request, body []byte, compressed bool, timeout time.Duration) error {
	if timeout > 0 {
		var deadline time.Time
		if timeout < 100*time.Millisecond {
			deadline = time.Now().Add(timeout)
		} else {
			deadline = time.Unix(0, c.cachedNow.Load()).Add(timeout)
		}
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return WrapError(ErrorTypeNetwork, "set write deadline failed", err)
		}
	}

	estimatedSize := len(req.headerBuf) + 512
	writeBuf := c.getWriteBuf(estimatedSize)
	defer c.putWriteBuf(writeBuf)

	var n int
	var encErr error
	for attempt := 0; attempt < 3; attempt++ {
		n, encErr = writeRequest(writeBuf, req, c.config, c.host, c.port, c.config.UseTLS, body, compressed, c.isForwardProxy, c.proxyAuthHeader)
		if encErr == nil {
			break
		}
		if encErr != ErrHeaderBufferSmall {
			if timeout > 0 {
				_ = conn.SetWriteDeadline(time.Time{})
			}
			return WrapError(ErrorTypeInternal, "encode request failed", encErr)
		}
		if attempt < 2 {
			needed := len(writeBuf) * 2
			if c.config.WriteBufferSize > 0 && needed > c.config.WriteBufferSize {
				needed = c.config.WriteBufferSize
			}
			c.putWriteBuf(writeBuf)
			writeBuf = c.getWriteBuf(needed)
		}
	}
	if encErr != nil {
		if timeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}
		return WrapError(ErrorTypeInternal, "encode request: buffer too small", encErr)
	}

	if err := writeAllBatched(conn, writeBuf[:n], body); err != nil {
		if timeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}
		return WrapError(ErrorTypeNetwork, "write request failed", err)
	}

	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return nil
}

// readResponse reads and parses one HTTP response from conn.
func (c *Connection) readResponse(conn net.Conn, resp *Response, timeout time.Duration, method string) error {
	if timeout > 0 {
		var deadline time.Time
		if timeout < 100*time.Millisecond {
			deadline = time.Now().Add(timeout)
		} else {
			deadline = time.Unix(0, c.cachedNow.Load()).Add(timeout)
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return WrapError(ErrorTypeNetwork, "set read deadline failed", err)
		}
	}

	readBuf := c.getReadBuf(c.readBufSize)
	defer c.putReadBuf(readBuf)

	err := readResponse(conn, readBuf, resp, c.maxRespSize, method, c.headerReadSize, c.bodyReadSize)
	if err != nil && timeout > 0 {
		_ = conn.SetReadDeadline(time.Time{})
	}
	if err == nil {
		err = decompressBody(resp)
	}
	return err
}

func (c *Connection) clearDeadlines(conn net.Conn) {
	if conn != nil {
		_ = conn.SetReadDeadline(time.Time{})
		_ = conn.SetWriteDeadline(time.Time{})
	}
}

func (c *Connection) updateTimeCache() {
	ctr := c.timeCounter.Add(1)
	if ctr&127 == 0 {
		now := time.Now().UnixNano()
		c.cachedNow.Store(now)
		c.lastUsed.Store(now)
	} else {
		c.lastUsed.Store(c.cachedNow.Load())
	}
}

// ctxDoneChan returns ctx.Done(), or nil if ctx is nil.
// Selecting on a nil channel blocks forever — used to safely skip ctx.Done() in select.
func ctxDoneChan(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// run reads responses in FIFO order, matching the order requests were written.
// Cancelled entries are still fully drained from the wire to keep the pipeline in sync.
// rr.br is a persistent bufio.Reader: leftover bytes from one response carry over
// to the next read, preventing data loss when the TCP layer batches multiple responses.
func (rr *ResponseReader) run() {
	connVal := rr.conn.conn.Load()
	if connVal == nil {
		return
	}
	connPtr := connVal.(*net.Conn)
	if connPtr == nil || *connPtr == nil {
		return
	}
	conn := *connPtr

	for {
		// Wait for work.
		select {
		case <-rr.stopChan:
			return
		case <-rr.conn.pendingSignal:
		}

		// Drain the entire pending queue before sleeping again.
		for {
			rr.conn.pendingMu.Lock()
			if len(rr.conn.pendingReqs) == 0 {
				rr.conn.pendingMu.Unlock()
				break
			}
			pending := rr.conn.pendingReqs[0]
			rr.conn.pendingMu.Unlock()

			if rr.conn.closing.Load() != 0 {
				return
			}

			resp := pending.req.resp
			if resp == nil {
				rr.conn.activeReqs.Add(-1)
				rr.conn.removePending(pending)
				if !pending.cancelled.Load() {
					select {
					case pending.err <- WrapError(ErrorTypeInternal, "response object is nil", ErrInvalidResponse):
					default:
					}
				}
				continue
			}
			resp.Reset()

			readTimeout := rr.conn.effectiveReadTimeout(pending.req)

			// Set deadline on the underlying TCP conn; read via the persistent
			// bufio.Reader so leftover bytes from the previous response are reused.
			if readTimeout > 0 {
				var dl time.Time
				if readTimeout < 100*time.Millisecond {
					dl = time.Now().Add(readTimeout)
				} else {
					dl = time.Unix(0, rr.conn.cachedNow.Load()).Add(readTimeout)
				}
				_ = conn.SetReadDeadline(dl)
			}
			err := readResponseBuffered(rr.br, rr.scratch, resp,
				rr.conn.maxRespSize, pending.req.Method)
			if err != nil && readTimeout > 0 {
				_ = conn.SetReadDeadline(time.Time{})
			}

			wasCancelled := pending.cancelled.Load()

			rr.conn.activeReqs.Add(-1)
			rr.conn.removePending(pending)

			if err != nil {
				if resp.StatusCode > 0 {
					rr.conn.clearDeadlines(conn)
					rr.conn.reqsOnConn.Add(1)
					rr.conn.updateTimeCache()
					connClose := resp.isConnectionClose()
					if !wasCancelled {
						select {
						case pending.response <- resp:
						default:
						}
					} else if pending.releaseFn != nil {
						pending.releaseFn(resp)
					}
					if connClose {
						rr.conn.closeConn()
						return
					}
					continue
				}
				// Real error — release resp if we own it.
				if wasCancelled && pending.releaseFn != nil {
					pending.releaseFn(resp)
				}
				if wasCancelled {
					// Caller already returned; close conn to reset state.
					rr.conn.closeConn()
					return
				}
				var netErr net.Error
				switch {
				case errors.As(err, &netErr) && netErr.Timeout():
					// A timeout corrupts the bufio cursor; close the connection.
					rr.conn.closeConn()
					select {
					case pending.err <- WrapError(ErrorTypeTimeout, "read timeout", err):
					default:
					}
					return
				case err == io.EOF:
					rr.conn.closeConn()
					select {
					case pending.err <- WrapError(ErrorTypeNetwork, "connection closed by server", err):
					default:
					}
					return
				default:
					rr.conn.closeConn()
					select {
					case pending.err <- err:
					default:
					}
					return
				}
			} else {
				rr.conn.clearDeadlines(conn)
				rr.conn.reqsOnConn.Add(1)
				rr.conn.updateTimeCache()

				connClose := resp.isConnectionClose()

				if !wasCancelled {
					select {
					case pending.response <- resp:
					default:
					}
				} else if pending.releaseFn != nil {
					pending.releaseFn(resp)
				}

				if connClose {
					rr.conn.closeConn()
					return
				}
			}
		}
	}
}
