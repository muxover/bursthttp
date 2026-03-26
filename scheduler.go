package client

import (
	"context"
	"sync"
	"sync/atomic"
)

// scheduledWork is a request submitted to a hostScheduler.
type scheduledWork struct {
	ctx     context.Context
	req     *Request
	poolKey string
	useTLS  bool
	resp    chan scheduledResult
}

type scheduledResult struct {
	resp *Response
	err  error
}

// hostScheduler owns a bounded request queue and a fixed worker pool for one host.
type hostScheduler struct {
	queue   chan scheduledWork
	stopCh  chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup
}

// Scheduler routes requests through per-host queues processed by a bounded
// worker pool. This replaces the spin-wait in GetConnection with a proper
// blocking queue, giving stable latency under overload and eliminating
// busy-wait CPU overhead when all connections are busy.
type Scheduler struct {
	client   *Client
	hosts    sync.Map // map[string]*hostScheduler
	workers  int
	queueCap int
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewScheduler creates a Scheduler backed by the given client.
// workers is the number of worker goroutines per host (default: PoolSize).
// queueCap is the max queued requests per host before Do blocks (default: workers*4).
func NewScheduler(client *Client, workers, queueCap int) *Scheduler {
	if workers <= 0 {
		workers = client.config.PoolSize
		if workers <= 0 {
			workers = 64
		}
	}
	if queueCap <= 0 {
		queueCap = workers * 4
	}
	return &Scheduler{
		client:   client,
		workers:  workers,
		queueCap: queueCap,
		stopCh:   make(chan struct{}),
	}
}

// Do submits req to the per-host scheduler and blocks until a response or error.
// poolKey is "scheme://host:port"; useTLS must match the scheme.
func (s *Scheduler) Do(ctx context.Context, req *Request, poolKey string, useTLS bool) (*Response, error) {
	hs := s.getOrCreateHostScheduler(poolKey, useTLS)

	work := scheduledWork{
		ctx:     ctx,
		req:     req,
		poolKey: poolKey,
		useTLS:  useTLS,
		resp:    make(chan scheduledResult, 1),
	}

	// Enqueue, respecting context cancellation.
	select {
	case hs.queue <- work:
	case <-ctx.Done():
		return nil, WrapError(ErrorTypeTimeout, "scheduler: enqueue cancelled", ctx.Err())
	case <-s.stopCh:
		return nil, WrapError(ErrorTypeNetwork, "scheduler: stopped", ErrConnectFailed)
	}

	// Wait for result.
	select {
	case result := <-work.resp:
		return result.resp, result.err
	case <-ctx.Done():
		return nil, WrapError(ErrorTypeTimeout, "scheduler: wait cancelled", ctx.Err())
	case <-s.stopCh:
		return nil, WrapError(ErrorTypeNetwork, "scheduler: stopped", ErrConnectFailed)
	}
}

// Stop drains the queues and shuts down all worker goroutines.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.hosts.Range(func(_, v interface{}) bool {
			hs := v.(*hostScheduler)
			if hs.stopped.CompareAndSwap(false, true) {
				close(hs.stopCh)
			}
			hs.wg.Wait()
			return true
		})
	})
}

func (s *Scheduler) getOrCreateHostScheduler(poolKey string, useTLS bool) *hostScheduler {
	if v, ok := s.hosts.Load(poolKey); ok {
		return v.(*hostScheduler)
	}
	hs := &hostScheduler{
		queue:  make(chan scheduledWork, s.queueCap),
		stopCh: make(chan struct{}),
	}
	if actual, loaded := s.hosts.LoadOrStore(poolKey, hs); loaded {
		return actual.(*hostScheduler)
	}
	// Start worker pool.
	for i := 0; i < s.workers; i++ {
		hs.wg.Add(1)
		go s.worker(hs, poolKey, useTLS)
	}
	return hs
}

func (s *Scheduler) worker(hs *hostScheduler, poolKey string, useTLS bool) {
	defer hs.wg.Done()
	for {
		select {
		case <-hs.stopCh:
			return
		case <-s.stopCh:
			return
		case work := <-hs.queue:
			// Check if caller already gave up.
			select {
			case <-work.ctx.Done():
				work.resp <- scheduledResult{err: WrapError(ErrorTypeTimeout, "scheduler: cancelled before execute", work.ctx.Err())}
				continue
			default:
			}

			conn := s.client.pool.GetConnection(work.poolKey, work.useTLS)
			if conn == nil {
				work.resp <- scheduledResult{err: WrapError(ErrorTypeNetwork, "scheduler: no connection available", ErrConnectFailed)}
				continue
			}
			work.req.ctx = work.ctx
			resp, err := conn.Do(work.req)
			work.resp <- scheduledResult{resp: resp, err: err}
		}
	}
}
