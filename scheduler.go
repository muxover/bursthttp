package client

import (
	"context"
	"sync"
	"sync/atomic"
)

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

type hostScheduler struct {
	queue   chan scheduledWork
	stopCh  chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup
}

type Scheduler struct {
	client   *Client
	hosts    sync.Map // map[string]*hostScheduler
	workers  int
	queueCap int
	stopCh   chan struct{}
	stopOnce sync.Once
}

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

func (s *Scheduler) Do(ctx context.Context, req *Request, poolKey string, useTLS bool) (*Response, error) {
	hs := s.getOrCreateHostScheduler(poolKey, useTLS)

	work := scheduledWork{
		ctx:     ctx,
		req:     req,
		poolKey: poolKey,
		useTLS:  useTLS,
		resp:    make(chan scheduledResult, 1),
	}

	select {
	case hs.queue <- work:
	case <-ctx.Done():
		return nil, WrapError(ErrorTypeTimeout, "scheduler: enqueue cancelled", ctx.Err())
	case <-s.stopCh:
		return nil, WrapError(ErrorTypeNetwork, "scheduler: stopped", ErrConnectFailed)
	}

	select {
	case result := <-work.resp:
		return result.resp, result.err
	case <-ctx.Done():
		return nil, WrapError(ErrorTypeTimeout, "scheduler: wait cancelled", ctx.Err())
	case <-s.stopCh:
		return nil, WrapError(ErrorTypeNetwork, "scheduler: stopped", ErrConnectFailed)
	}
}

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
