package api

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Every tenant-configured delivery — an S3 bucket notification
// (NotificationDispatcher) or a webhook (emitEvent, a batch's deleteFanout)
// — runs on ONE process-wide pool: deliveryWorkers goroutines pulling from
// one FIFO queue of deliveryQueueSize jobs, so jobs START in the order they
// were submitted (a batch submits its keys in request order). Before
// (post-merge review of 2a, 2026-10-08) a single PUT/GET/DELETE started a
// goroutine per notification and per event, and a batch delete 4 workers of
// its own: with a blackholed target a 1,000-key batch drained for ~42 min
// and concurrent batches piled up without limit (~16,800 goroutines at 100
// batches/min).
//
// Past the queue nothing waits: a webhook job is recorded at once in
// webhook_deliveries as failed `dropped: overloaded` (GET /api/v1/events
// and the deliveries API stay the source of truth), a notification job is
// logged and counted (bucket notifications keep no delivery log). Each job
// has its own deadline — notificationDeliveryTimeout for a notification,
// webhookDeliveryTimeout for a webhook (#637 had both share one 10 s
// context per key: two hanging notification targets used it up and the
// webhook got no POST and no row).
//
// A stopping process drains the pool (Server.Shutdown → drainDeliveries,
// after the long operations): it waits up to deliveryDrainBound for the
// queue and the jobs in flight, then records every job still queued as
// failed `dropped: shutdown`, cuts the ones in flight (they record their
// own failure) and logs the count.

const (
	deliveryWorkers             = 32
	deliveryQueueSize           = 4096
	notificationDeliveryTimeout = 5 * time.Second
	webhookDeliveryTimeout      = 10 * time.Second
	// deliveryDrainBound is how long a stopping process waits for pending
	// deliveries (never more than what is left of longOpDrainBound): a
	// customer's dead webhook must not hold a deploy for 15 min.
	deliveryDrainBound = 30 * time.Second
	// deliveryCancelGrace lets the cut jobs write their failure rows.
	deliveryCancelGrace = 5 * time.Second
)

const (
	deliveryKindNotification = "notification"
	deliveryKindWebhook      = "webhook"
	deliveryDroppedOverload  = "dropped: overloaded"
	deliveryDroppedShutdown  = "dropped: shutdown"
)

var deliveriesDropped = func() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_event_deliveries_dropped_total",
		Help: "Bucket notifications and webhooks never attempted: the process-wide delivery queue was full (overloaded) or the process was stopping (shutdown). A dropped webhook has a failed webhook_deliveries row.",
	}, []string{"kind", "reason"})
	for _, k := range []string{deliveryKindNotification, deliveryKindWebhook} {
		for _, r := range []string{"overloaded", "shutdown"} {
			c.WithLabelValues(k, r)
		}
	}
	return c
}()

var deliveriesPending = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "vaultaire_event_deliveries_pending",
	Help: "Bucket notifications and webhooks queued or being delivered by the process-wide delivery pool.",
}, func() float64 { return float64(eventDeliveries.pending()) })

// droppedDelivery is the webhook_deliveries row a webhook job owes when it
// never runs.
type droppedDelivery struct{ webhookID, eventID string }

// deliveryJob is one delivery: a notification to a bucket's targets, or one
// event to a tenant's webhooks.
type deliveryJob struct {
	kind string
	db   *sql.DB
	// run delivers; ctx is the pool's (cut at the drain bound) — the job
	// sets its own deadline.
	run func(ctx context.Context)
	// owed lists the rows of a webhook job that never runs (nil for a
	// notification).
	owed func(ctx context.Context) []droppedDelivery
}

type deliveryPool struct {
	workers int
	queue   chan deliveryJob
	logger  *zap.Logger

	startOnce   sync.Once
	base        context.Context
	cancelBase  context.CancelFunc
	mu          sync.Mutex
	closed      bool
	outstanding atomic.Int64 // accepted and not finished
}

// eventDeliveries is the process's pool.
var eventDeliveries = newDeliveryPool(deliveryWorkers, deliveryQueueSize)

func newDeliveryPool(workers, queue int) *deliveryPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &deliveryPool{workers: workers, queue: make(chan deliveryJob, queue), base: ctx, cancelBase: cancel}
}

func (p *deliveryPool) log() *zap.Logger {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logger == nil {
		return zap.NewNop()
	}
	return p.logger
}

func (p *deliveryPool) startWorkers() {
	p.startOnce.Do(func() {
		for i := 0; i < p.workers; i++ {
			go p.work() // #nosec G118 -- the pool's fixed worker set, detached from any request on purpose
		}
	})
}

func (p *deliveryPool) work() {
	for j := range p.queue {
		p.runJob(j)
	}
}

func (p *deliveryPool) runJob(j deliveryJob) {
	defer p.outstanding.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			p.log().Error("event delivery panicked", zap.Any("panic", r), zap.String("kind", j.kind))
		}
	}()
	j.run(p.base)
}

// pending is how many jobs are queued or running.
func (p *deliveryPool) pending() int {
	if p == nil {
		return 0
	}
	return int(p.outstanding.Load())
}

// submit queues jobs in order without blocking; every job the queue cannot
// take (full, or the process stopping) is recorded as dropped at once.
func (p *deliveryPool) submit(jobs ...deliveryJob) {
	p.startWorkers()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	var dropped []deliveryJob
	reason := deliveryDroppedOverload
	if closed {
		dropped, reason = jobs, deliveryDroppedShutdown
	} else {
		for i, j := range jobs {
			p.outstanding.Add(1)
			select {
			case p.queue <- j:
				continue
			default:
			}
			p.outstanding.Add(-1)
			dropped = jobs[i:]
			break
		}
	}
	p.recordDropped(dropped, reason)
}

// recordDropped writes the failure rows of jobs that will never run, in one
// statement per database, and counts them.
func (p *deliveryPool) recordDropped(jobs []deliveryJob, reason string) {
	if len(jobs) == 0 {
		return
	}
	label := "overloaded"
	if reason == deliveryDroppedShutdown {
		label = "shutdown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows := map[*sql.DB][]droppedDelivery{}
	notifications := 0
	for _, j := range jobs {
		deliveriesDropped.WithLabelValues(j.kind, label).Inc()
		if j.kind == deliveryKindNotification {
			notifications++
		}
		if j.owed != nil && j.db != nil {
			rows[j.db] = append(rows[j.db], j.owed(ctx)...)
		}
	}
	if notifications > 0 {
		p.log().Warn("bucket notifications dropped", zap.Int("count", notifications), zap.String("reason", reason))
	}
	for db, rs := range rows {
		if err := insertDroppedDeliveries(ctx, db, rs, reason); err != nil {
			p.log().Error("dropped webhook deliveries not recorded", zap.Error(err), zap.Int("rows", len(rs)), zap.String("reason", reason))
		}
	}
}

func insertDroppedDeliveries(ctx context.Context, db *sql.DB, rows []droppedDelivery, reason string) error {
	for len(rows) > 0 {
		n := min(len(rows), 1000)
		ids, hooks, events := make([]string, n), make([]string, n), make([]string, n)
		for i, r := range rows[:n] {
			ids[i], hooks[i], events[i] = uuid.New().String(), r.webhookID, r.eventID
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, response_code, response_body, latency_ms)
			SELECT u.id, u.hook, u.event, 'failed', 0, $4, 0
			FROM unnest($1::text[], $2::text[], $3::text[]) AS u(id, hook, event)`,
			pq.Array(ids), pq.Array(hooks), pq.Array(events), reason); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

// drain is the stop sequence's wait: up to bound for every accepted job,
// then the queued ones are recorded as dropped at shutdown and the running
// ones cut. Returns how many queued jobs were dropped. Nothing new starts
// afterwards: a later submit is recorded as dropped at once.
func (p *deliveryPool) drain(bound time.Duration) int {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	deadline := time.Now().Add(bound)
	for p.outstanding.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p.outstanding.Load() == 0 {
		return 0
	}
	// The queue first, then the cut: a worker freed by the cut must find
	// nothing left to start.
	var left []deliveryJob
	for empty := false; !empty; {
		select {
		case j := <-p.queue:
			left = append(left, j)
		default:
			empty = true
		}
	}
	p.outstanding.Add(-int64(len(left)))
	inFlight := p.outstanding.Load()
	p.cancelBase()
	p.recordDropped(left, deliveryDroppedShutdown)
	p.log().Warn("event deliveries dropped at shutdown",
		zap.Int("dropped", len(left)), zap.Int64("in_flight_cut", inFlight), zap.Duration("waited", bound))
	graceEnd := time.Now().Add(deliveryCancelGrace)
	for p.outstanding.Load() > 0 && time.Now().Before(graceEnd) {
		time.Sleep(10 * time.Millisecond)
	}
	return len(left)
}

// drainDeliveries is Server.Shutdown's call: what is left of the long-op
// budget (measured from the stop's start), at most deliveryDrainBound.
func (s *Server) drainDeliveries(stopStarted time.Time) int {
	p := eventDeliveries
	p.mu.Lock()
	if p.logger == nil {
		p.logger = s.log()
	}
	p.mu.Unlock()
	bound := min(deliveryDrainBound, time.Until(stopStarted.Add(longOpDrainBound)))
	return p.drain(max(bound, time.Second))
}

// webhookJob delivers one recorded event to the endpoints given, with its
// own deadline; never run, it owes a failed row per matching endpoint.
func webhookJob(db *sql.DB, logger *zap.Logger, endpoints []webhookEndpoint, eventID, eventType, tenantID string, payload []byte) deliveryJob {
	return deliveryJob{
		kind: deliveryKindWebhook,
		db:   db,
		run: func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, webhookDeliveryTimeout)
			defer cancel()
			deliverToEndpoints(ctx, db, logger, endpoints, eventID, eventType, tenantID, payload)
		},
		owed: func(context.Context) []droppedDelivery { return owedRows(endpoints, eventID, eventType) },
	}
}

func owedRows(endpoints []webhookEndpoint, eventID, eventType string) []droppedDelivery {
	var out []droppedDelivery
	for _, ep := range endpoints {
		if matchesWebhookFilter(ep.filter, eventType) {
			out = append(out, droppedDelivery{webhookID: ep.id, eventID: eventID})
		}
	}
	return out
}
