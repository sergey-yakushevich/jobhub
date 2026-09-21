package jev

import (
	"context"
	"log"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// Scorer is the background loop between ingest and the board: ingest hands it
// the ids of rows it just ADDED, and each gets one Evaluate call. Existing
// rows are never enqueued and never re-scored — the historical board keeps
// whatever it has (no backfill), and a re-pushed lead keeps its verdict.
type Scorer struct {
	Client *Client
	Store  *store.Store
	// Concurrency is how many Evaluate calls may be in flight. Two is plenty:
	// a sweep pushes tens of leads and a call is sub-second.
	Concurrency int
	queue       chan int64
}

// NewScorer sizes the queue for the largest sweep the board has ever seen,
// times ten. Enqueue drops (with a log line) rather than blocks if it fills —
// scoring is an enrichment, and ingest must never wait on it.
func NewScorer(c *Client, st *store.Store) *Scorer {
	return &Scorer{Client: c, Store: st, Concurrency: 2, queue: make(chan int64, 2048)}
}

// Enqueue accepts the ids of freshly added leads. Non-blocking by design.
func (s *Scorer) Enqueue(ids []int64) {
	for _, id := range ids {
		select {
		case s.queue <- id:
		default:
			log.Printf("[jev] queue full, dropping lead %d (it stays unscored)", id)
		}
	}
}

// Start launches the workers. They live for the process; ctx is for tests.
func (s *Scorer) Start(ctx context.Context) {
	n := s.Concurrency
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		go s.run(ctx)
	}
}

func (s *Scorer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.queue:
			if err := s.scoreOne(ctx, id); err != nil {
				log.Printf("[jev] lead %d: %v", id, err)
			}
		}
	}
}

// scoreOne loads, judges and writes back one lead. One call carries the whole
// scorecard — gates and axes — because Jev's server time is flat per request.
func (s *Scorer) scoreOne(ctx context.Context, id int64) error {
	j, err := s.Store.JobByID(id)
	if err != nil {
		return err
	}
	// Belt and braces on the no-backfill/no-rescore rule: even a doubly
	// enqueued id scores at most once.
	if j.Workable != "" {
		return nil
	}
	sc, ok := scorecards[j.Profile]
	if !ok {
		// No scorecard for this seeker: the axes are per-person and guessing
		// them would rank her leads with someone else's weights.
		return nil
	}
	state := buildState(sc, j, time.Now())
	res, err := s.evaluateWithRetry(ctx, state, questionsFor(sc))
	if err != nil {
		return err
	}
	v := compute(sc, j, res)
	return s.Store.SetJobFit(id, v.Fit, v.Workable, v.Detail, time.Now())
}

// evaluateWithRetry gives transient failures one second and one more chance.
func (s *Scorer) evaluateWithRetry(ctx context.Context, state string, qs map[string]Question) (*Result, error) {
	res, err := s.Client.Evaluate(ctx, state, qs)
	if err == nil {
		return res, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Second):
	}
	return s.Client.Evaluate(ctx, state, qs)
}
