package client

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	initialConnectionReadBufSize  = 16 * 1024
	initialConnectionWriteBufSize = 16 * 1024
	healthWindowSize              = 50 // requests per health-score window
)

var (
	connectionReadBufPool = sync.Pool{
		New: func() interface{} { return make([]byte, initialConnectionReadBufSize) },
	}
	connectionWriteBufPool = sync.Pool{
		New: func() interface{} { return make([]byte, initialConnectionWriteBufSize) },
	}

	// resp ownership transferred to ResponseReader; DoWithContext must not release it
	errRespTransferred = errors.New("internal: resp ownership transferred to pipeline reader")
)

type PendingRequest struct {
	req         *Request
	response    chan *Response
	err         chan error
	cancelled   atomic.Bool // true when caller's context fired
	releaseFn   func(*Response)
	sentAt      int64 // UnixNano when request was written to wire
	resp        *Response     // snapshotted at enqueue; req may be released before ResponseReader runs
	readTimeout time.Duration
	method      string
}

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

	latencyEWMA   atomic.Int64 // nanoseconds, EWMA α=1/8
	errorCount    atomic.Int32 // recent error count (reset every healthWindowSize requests)
	totalWindow   atomic.Int32 // request count in current health window
	healthScore   atomic.Int32 // 0–100; higher is better
	pipelineDepth atomic.Int32 // dynamic pipeline depth (auto-tuned when EnablePipelineAutoTune)

	bytesRead    atomic.Int64
	bytesWritten atomic.Int64

	connectMu sync.Mutex // serializes reconnection attempts

	pendingReqs    []*PendingRequest
	pendingMu      sync.Mutex
	responseReader *ResponseReader
	readerStopped  atomic.Int32
	closing        atomic.Int32
	pendingSignal  chan struct{}

	readBufSize    int
	writeBufSize   int
	maxRespSize    int
	maxReqSize     int
	headerReadSize int
	bodyReadSize   int

	isForwardProxy  bool
	proxyAuthHeader []byte
}

type ResponseReader struct {
	conn     *Connection
	br       *bufio.Reader // wraps the TCP conn; preserves inter-response leftover
	scratch  []byte        // scratch buffer for header parsing
	stopChan chan struct{}
}

// TCP is not established until first use.
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
	depth := int32(config.MaxPipelinedRequests)
	if depth <= 0 {
		depth = 10
	}
	c.pipelineDepth.Store(depth)
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

func (c *Connection) IsHealthy() bool {
	return c.healthy.Load() == 1 && c.stopped.Load() == 0
}

func (c *Connection) CanAcceptRequest() bool {
	if !c.IsHealthy() {
		return false
	}
	if c.config.EnablePipelining {
		max := int(c.pipelineDepth.Load())
		if max <= 0 {
			max = 10
		}
		return int(c.activeReqs.Load()) < max
	}
	return c.activeReqs.Load() == 0
}

func (c *Connection) recordSuccess(elapsed time.Duration) {
	if !c.config.EnableHealthScoring {
		return
	}
	ns := elapsed.Nanoseconds()
	old := c.latencyEWMA.Load()
	var newEWMA int64
	if old == 0 {
		newEWMA = ns
	} else {
		// α = 1/8: newEWMA = old + (sample - old) / 8
		newEWMA = old + (ns-old)>>3
	}
	c.latencyEWMA.Store(newEWMA)
	c.advanceHealthWindow(false)
	if c.config.EnablePipelineAutoTune && c.config.EnablePipelining {
		c.tunePipelineDepth(newEWMA)
	}
}

func (c *Connection) tunePipelineDepth(ewmaNs int64) {
	ms := ewmaNs / 1e6
	cur := c.pipelineDepth.Load()
	maxDepth := int32(c.config.MaxPipelinedRequests)
	if maxDepth <= 0 {
		maxDepth = 10
	}
	const hardCap = 32
	if maxDepth > hardCap {
		maxDepth = hardCap
	}

	var target int32
	switch {
	case ms < 10:
		target = cur + 1
		if target > maxDepth {
			target = maxDepth
		}
	case ms > 100:
		target = cur - 1
		if target < 1 {
			target = 1
		}
	default:
		return // 10–100ms: keep current depth
	}
	c.pipelineDepth.Store(target)
}

func (c *Connection) recordError() {
	if !c.config.EnableHealthScoring {
		return
	}
	c.errorCount.Add(1)
	c.advanceHealthWindow(true)
}

func (c *Connection) advanceHealthWindow(isError bool) {
	_ = isError
	total := c.totalWindow.Add(1)
	if total < healthWindowSize {
		return
	}
	errors := c.errorCount.Load()
	c.errorCount.Store(0)
	c.totalWindow.Store(0)

	score := int32(100)

	latMs := c.latencyEWMA.Load() / 1e6
	if latMs > 50 {
		penalty := (latMs - 50) / 10
		if penalty > 60 {
			penalty = 60
		}
		score -= int32(penalty)
	}

	if errors > 0 {
		errPenalty := int32(40) * errors / int32(healthWindowSize)
		score -= errPenalty
	}

	if score < 0 {
		score = 0
	}
	c.healthScore.Store(score)
}

func (c *Connection) HealthScore() int32 {
	s := c.healthScore.Load()
	if s == 0 && c.latencyEWMA.Load() == 0 {
		return 100 // fresh connection
	}
	return s
}

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
	reqStart := time.Now()

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
			c.recordError()
			return nil, WrapError(ErrorTypeTimeout, "read timeout", err)
		}
		if err == io.EOF {
			c.healthy.Store(0)
			c.recordError()
			return nil, WrapError(ErrorTypeNetwork, "connection closed by server", err)
		}
		c.healthy.Store(0)
		c.recordError()
		return nil, err
	}

	c.clearDeadlines(conn)
	c.reqsOnConn.Add(1)
	c.updateTimeCache()
	c.recordSuccess(time.Since(reqStart))
	c.bytesWritten.Add(int64(len(body)))
	c.bytesRead.Add(int64(resp.ContentLength))

	if resp.isConnectionClose() {
		c.healthy.Store(0)
	}

	return resp, nil
}

func (c *Connection) doPipelined(req *Request) (*Response, error) {
	if c.closing.Load() != 0 {
		return nil, WrapError(ErrorTypeNetwork, "connection is closing", ErrConnectFailed)
	}

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
		req:         req,
		response:    make(chan *Response, 1),
		err:         make(chan error, 1),
		resp:        req.resp,
		readTimeout: c.effectiveReadTimeout(req),
		method:      req.Method,
	}

	c.pendingMu.Lock()
	if c.closing.Load() != 0 {
		c.pendingMu.Unlock()
		return nil, WrapError(ErrorTypeNetwork, "connection is closing", ErrConnectFailed)
	}

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
	pending.sentAt = time.Now().UnixNano()
	c.bytesWritten.Add(int64(len(body)))
	c.pendingReqs = append(c.pendingReqs, pending)
	c.pendingMu.Unlock()

	select {
	case c.pendingSignal <- struct{}{}:
	default:
	}

	ctxDone := ctxDoneChan(req.ctx)
	select {
	case resp := <-pending.response:
		return resp, nil
	case err := <-pending.err:
		return nil, err
	case <-ctxDone:
		pending.releaseFn = req.releaseFn
		pending.cancelled.Store(true)
		return nil, errRespTransferred
	}
}

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

// Must be called with pendingMu held.
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

// single attempt only — retrying here multiplies proxy CONNECT timeouts
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

func (c *Connection) Stop() {
	if !c.stopped.CompareAndSwap(0, 1) {
		return
	}
	c.closeConn()
}

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

	if len(req.PreEncodedHeaderPrefix) > 0 {
		var clLine [64]byte
		n := copy(clLine[:], "Content-Length: ")
		n += copy(clLine[n:], strconv.Itoa(len(body)))
		n += copy(clLine[n:], "\r\n\r\n")
		buffers := net.Buffers{req.PreEncodedHeaderPrefix, clLine[:n], body}
		if _, err := buffers.WriteTo(conn); err != nil {
			if timeout > 0 {
				_ = conn.SetWriteDeadline(time.Time{})
			}
			return WrapError(ErrorTypeNetwork, "write pre-encoded request failed", err)
		}
		if timeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}
		return nil
	}

	part1Buf := c.getWriteBuf(512)
	defer c.putWriteBuf(part1Buf)
	p1, part1Err := writeRequestPart1(part1Buf, req, c.config, c.host, c.port, c.config.UseTLS, compressed, c.isForwardProxy)
	if part1Err != nil {
		if timeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}
		return WrapError(ErrorTypeInternal, "encode request part1 failed", part1Err)
	}
	var part3Buf [128]byte
	p3, part3Err := writeRequestPart3(part3Buf[:], req, len(body), c.proxyAuthHeader)
	if part3Err != nil {
		if timeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}
		return WrapError(ErrorTypeInternal, "encode request part3 failed", part3Err)
	}
	var headerBlock []byte
	if req.headerLen > 0 {
		headerBlock = req.headerBuf[:req.headerLen]
	}
	buffers := net.Buffers{part1Buf[:p1], headerBlock, part3Buf[:p3]}
	if _, err := buffers.WriteTo(conn); err != nil {
		if timeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}
		return WrapError(ErrorTypeNetwork, "write request failed", err)
	}
	if len(body) > 0 {
		if err := writeAll(conn, body); err != nil {
			if timeout > 0 {
				_ = conn.SetWriteDeadline(time.Time{})
			}
			return WrapError(ErrorTypeNetwork, "write body failed", err)
		}
	}

	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return nil
}

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

// nil ctx.Done() blocks forever in select — safe no-op when context is absent.
func ctxDoneChan(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

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
		select {
		case <-rr.stopChan:
			return
		case <-rr.conn.pendingSignal:
		}

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

			resp := pending.resp
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

			readTimeout := pending.readTimeout

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
				rr.conn.maxRespSize, pending.method)
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
				rr.conn.recordError()
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
				if pending.sentAt > 0 {
					rr.conn.recordSuccess(time.Duration(time.Now().UnixNano() - pending.sentAt))
				}
				rr.conn.bytesRead.Add(int64(resp.ContentLength))

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
