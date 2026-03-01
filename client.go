package client

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// GetVersion returns the library version.
func GetVersion() string {
	return Version
}

// Client is the main HTTP client. Safe for concurrent use.
//
// A single Client is designed for requests to one primary host (configured
// via Config.Host / Config.Port). For multi-host usage, set Request.URL and
// the client will route to the correct host pool automatically.
type Client struct {
	config        *Config
	pool          *Pool
	dialer        *Dialer
	tlsConfig     *TLSConfig
	compressor    *Compressor
	metrics       MetricsCollector
	retryer       *Retryer
	requestPool   *sync.Pool
	responsePool  *sync.Pool
	enableLogging bool
}

// NewClient creates a new HTTP client with the given configuration.
// Pass nil to use DefaultConfig.
func NewClient(config *Config) (*Client, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := config.Validate(); err != nil {
		return nil, LogErrorWithFlag(ErrorTypeValidation, "config validation failed", err, nil, config.EnableLogging)
	}

	dialer, err := NewDialer(config)
	if err != nil {
		return nil, err
	}

	var tlsConfig *TLSConfig
	if config.UseTLS {
		tlsConfig = NewTLSConfig(config)
	}

	var compressor *Compressor
	if config.EnableCompression {
		compressor = NewCompressor(config.CompressionLevel)
	}

	pool := NewPool(config, dialer, tlsConfig, compressor)

	headerBufSize := config.HeaderBufferSize
	if headerBufSize <= 0 {
		headerBufSize = initialHeaderBufferSize
	}
	bodyBufSize := initialRequestBodyBufferSize
	if config.MaxRequestBodySize > 0 && config.MaxRequestBodySize < bodyBufSize {
		bodyBufSize = config.MaxRequestBodySize
	}

	reqPool := &sync.Pool{
		New: func() interface{} {
			return &Request{
				headerBuf:   make([]byte, headerBufSize),
				bodyBuf:     make([]byte, bodyBufSize),
				maxBodySize: config.MaxRequestBodySize,
			}
		},
	}

	respBufSize := initialResponseBufferSize
	if config.MaxResponseBodySize > 0 && config.MaxResponseBodySize < respBufSize {
		respBufSize = config.MaxResponseBodySize
	}

	respPool := &sync.Pool{
		New: func() interface{} {
			return &Response{
				bodyBuf: make([]byte, respBufSize),
				Headers: make([]Header, 0, 16),
			}
		},
	}

	var mc MetricsCollector
	if config.Metrics != nil {
		mc = config.Metrics
	}

	return &Client{
		config:        config,
		pool:          pool,
		dialer:        dialer,
		tlsConfig:     tlsConfig,
		compressor:    compressor,
		metrics:       mc,
		retryer:       NewRetryer(config),
		requestPool:   reqPool,
		responsePool:  respPool,
		enableLogging: config.EnableLogging,
	}, nil
}

// Start pre-establishes connections to the primary host.
// The number of connections created is min(n, PoolSize) where n defaults to
// PoolSize/4 when no argument is given. Returns the number of connections
// established. Calling Start is optional — connections are created lazily on
// first request when Start is not called.
func (c *Client) Start() error {
	return c.StartN(c.config.PoolSize / 4)
}

// StartN pre-establishes n connections to the primary host.
func (c *Client) StartN(n int) error {
	if n <= 0 {
		n = 1
	}
	if n > c.config.PoolSize {
		n = c.config.PoolSize
	}
	scheme := "http"
	if c.config.UseTLS {
		scheme = "https"
	}
	key := fmt.Sprintf("%s://%s:%d", scheme, c.config.Host, c.config.Port)
	created := c.pool.WarmUp(key, c.config.UseTLS, n)
	if created == 0 {
		return WrapError(ErrorTypeNetwork, "warm-up: could not establish any connections", ErrConnectFailed)
	}
	return nil
}

// Stop closes all pooled connections immediately.
func (c *Client) Stop() {
	c.pool.Stop()
	c.dialer.Stop()
}

// GracefulStop waits for in-flight requests to complete (up to timeout)
// before closing all connections. Returns true if all requests drained.
func (c *Client) GracefulStop(timeout time.Duration) bool {
	return c.pool.GracefulStop(timeout)
}

// Do executes an HTTP request using context.Background().
func (c *Client) Do(req *Request) (*Response, error) {
	return c.DoWithContext(context.Background(), req)
}

// DoWithContext executes an HTTP request with the given context.
// If retries are configured, transient failures are retried automatically.
func (c *Client) DoWithContext(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, WrapError(ErrorTypeValidation, "request is nil", ErrInvalidResponse)
	}

	req.ctx = ctx

	if ctxDone := ctxDoneChan(req.ctx); ctxDone != nil {
		select {
		case <-ctxDone:
			return nil, WrapError(ErrorTypeTimeout, "request cancelled before send", req.ctx.Err())
		default:
		}
	}

	host, dialPort, useTLS := c.config.Host, c.config.Port, c.config.UseTLS

	if req.URL != "" {
		parsedHost, parsedPort, parsedTLS, parsedPath, ok := req.resolveURL()
		if !ok {
			return nil, WrapError(ErrorTypeValidation, "invalid request URL: "+req.URL, ErrInvalidURL)
		}
		host = parsedHost
		dialPort = parsedPort
		useTLS = parsedTLS
		req.parsedPath = parsedPath
	}

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	poolKey := fmt.Sprintf("%s://%s:%d", scheme, host, dialPort)

	maxAttempts := 1
	if c.retryer != nil {
		maxAttempts = 1 + c.retryer.maxRetries
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if bm, ok := c.metrics.(*BuiltinMetrics); ok {
				bm.RecordRetry()
			}
			if c.retryer != nil {
				if err := c.retryer.Wait(ctx, attempt-1); err != nil {
					return nil, WrapError(ErrorTypeTimeout, "retry wait cancelled", err)
				}
			}
			req.urlParsed = false
			if req.URL != "" {
				req.resolveURL()
			}
		}

		var start time.Time
		if c.metrics != nil {
			start = c.metrics.RecordRequest(req.Method, host)
		}

		resp := c.acquireResponse()
		req.resp = resp

		conn := c.pool.GetConnection(poolKey, useTLS)
		if conn == nil {
			c.releaseResponse(resp)
			lastErr = LogErrorWithFlag(ErrorTypeInternal, "failed to get connection from pool", ErrConnectFailed, nil, c.enableLogging)
			if c.metrics != nil {
				c.metrics.RecordResponse(req.Method, host, 0, lastErr, start, 0, 0)
			}
			if c.retryer != nil && c.retryer.ShouldRetry(attempt, nil, lastErr) {
				continue
			}
			return nil, lastErr
		}

		req.releaseFn = c.releaseResponse
		resultResp, err := conn.Do(req)
		if err != nil {
			if err == errRespTransferred {
				return nil, WrapError(ErrorTypeTimeout, "request cancelled", req.ctx.Err())
			}
			if resultResp != nil && resultResp.StatusCode > 0 {
				if c.metrics != nil {
					c.metrics.RecordResponse(req.Method, host, resultResp.StatusCode, err, start, 0, int64(resultResp.ContentLength))
				}
				if c.retryer != nil && c.retryer.ShouldRetry(attempt, resultResp, err) {
					c.releaseResponse(resultResp)
					lastErr = err
					continue
				}
				return resultResp, err
			}
			c.releaseResponse(resp)
			if c.metrics != nil {
				c.metrics.RecordResponse(req.Method, host, 0, err, start, 0, 0)
			}
			lastErr = err
			if c.retryer != nil && c.retryer.ShouldRetry(attempt, nil, err) {
				continue
			}
			return nil, err
		}

		if c.metrics != nil {
			c.metrics.RecordResponse(req.Method, host, resultResp.StatusCode, nil, start, int64(len(req.Body)), int64(resultResp.ContentLength))
		}

		if c.retryer != nil && c.retryer.ShouldRetry(attempt, resultResp, nil) {
			c.releaseResponse(resultResp)
			continue
		}

		return resultResp, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, WrapError(ErrorTypeInternal, "max retries exhausted", ErrConnectFailed)
}

// DoStreaming executes a request and returns a StreamingResponse whose Body
// is an io.ReadCloser. The caller MUST call StreamingResponse.Close() when
// done to return the underlying response to the pool.
func (c *Client) DoStreaming(ctx context.Context, req *Request) (*StreamingResponse, error) {
	resp, err := c.DoWithContext(ctx, req)
	if err != nil {
		return nil, err
	}
	sr := &StreamingResponse{
		StatusCode:    resp.StatusCode,
		ContentLength: resp.ContentLength,
		Headers:       resp.Headers,
		Body:          newBodyReader(resp.Body),
		resp:          resp,
		releaseFn:     c.releaseResponse,
	}
	return sr, nil
}

// DoReader executes a request whose body is read from an io.Reader.
// contentLength must be the exact number of bytes that bodyReader will produce.
func (c *Client) DoReader(ctx context.Context, method, urlOrPath string, bodyReader io.Reader, contentLength int64, headers []Header) (*Response, error) {
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(bodyReader, body); err != nil {
		return nil, WrapError(ErrorTypeValidation, "failed to read request body", err)
	}
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = method
	if len(urlOrPath) > 0 && (len(urlOrPath) > 8 && (urlOrPath[:7] == "http://" || urlOrPath[:8] == "https://")) {
		req.URL = urlOrPath
	} else {
		req.Path = urlOrPath
	}
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.DoWithContext(ctx, req)
}

// Get performs a GET request to path using the configured host.
func (c *Client) Get(path string, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "GET"
	req.Path = path
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// GetURL performs a GET request to a full URL.
func (c *Client) GetURL(rawURL string, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "GET"
	req.URL = rawURL
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// Post performs a POST request to path with the given body.
func (c *Client) Post(path string, body []byte, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "POST"
	req.Path = path
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// PostURL performs a POST request to a full URL.
func (c *Client) PostURL(rawURL string, body []byte, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "POST"
	req.URL = rawURL
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// Put performs a PUT request to path.
func (c *Client) Put(path string, body []byte, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "PUT"
	req.Path = path
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// Patch performs a PATCH request to path.
func (c *Client) Patch(path string, body []byte, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "PATCH"
	req.Path = path
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// Delete performs a DELETE request to path.
func (c *Client) Delete(path string, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "DELETE"
	req.Path = path
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// Head performs a HEAD request to path.
func (c *Client) Head(path string, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "HEAD"
	req.Path = path
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// Options performs an OPTIONS request to path.
func (c *Client) Options(path string, headers []Header) (*Response, error) {
	req := c.acquireRequest()
	defer c.releaseRequest(req)
	req.Method = "OPTIONS"
	req.Path = path
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	return c.Do(req)
}

// AcquireRequest gets a Request from the client's pool.
func (c *Client) AcquireRequest() *Request {
	return c.acquireRequest()
}

// ReleaseRequest returns a Request to the client's pool.
func (c *Client) ReleaseRequest(req *Request) {
	c.releaseRequest(req)
}

// ReleaseResponse returns a Response to the client's pool.
// Always call this after processing a response to avoid memory leaks.
func (c *Client) ReleaseResponse(resp *Response) {
	c.releaseResponse(resp)
}

func (c *Client) acquireRequest() *Request {
	req := c.requestPool.Get().(*Request)
	req.Reset()
	req.maxBodySize = c.config.MaxRequestBodySize
	return req
}

func (c *Client) releaseRequest(req *Request) {
	if req == nil {
		return
	}
	headerBufSize := c.config.HeaderBufferSize
	if headerBufSize <= 0 {
		headerBufSize = initialHeaderBufferSize
	}
	if len(req.bodyBuf) > initialRequestBodyBufferSize*2 {
		req.bodyBuf = make([]byte, initialRequestBodyBufferSize)
	}
	if len(req.headerBuf) > headerBufSize*2 {
		req.headerBuf = make([]byte, headerBufSize)
	}
	c.requestPool.Put(req)
}

func (c *Client) acquireResponse() *Response {
	resp := c.responsePool.Get().(*Response)
	resp.Reset()
	resp.maxSize = c.config.MaxResponseBodySize
	if resp.maxSize > 0 && len(resp.bodyBuf) > resp.maxSize {
		resp.bodyBuf = make([]byte, resp.maxSize)
	} else if len(resp.bodyBuf) < initialResponseBufferSize {
		resp.bodyBuf = make([]byte, initialResponseBufferSize)
	}
	return resp
}

func (c *Client) releaseResponse(resp *Response) {
	if resp == nil {
		return
	}
	if len(resp.bodyBuf) > initialResponseBufferSize*2 {
		resp.bodyBuf = make([]byte, initialResponseBufferSize)
	}
	c.responsePool.Put(resp)
}

// GetHealthyConnections returns the total number of healthy pooled connections.
func (c *Client) GetHealthyConnections() int {
	return c.pool.GetHealthyConnections()
}

// Stats returns a snapshot of pool and metrics state.
func (c *Client) Stats() ClientStats {
	stats := ClientStats{
		HealthyConnections: c.pool.GetHealthyConnections(),
	}
	if c.metrics != nil {
		stats.Metrics = c.metrics.Snapshot()
	}
	return stats
}

// ClientStats holds a point-in-time snapshot of client state.
type ClientStats struct {
	HealthyConnections int
	Metrics            MetricsSnapshot
}
