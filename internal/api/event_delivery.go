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
	deliveryWorkers = 32
	// deliveryQueueSize: one tenant may queue a quarter of it — 2,048 jobs,
	// a full 1,000-key batch's notification + webhook per key.
	deliveryQueueSize           = 8192
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
	// deliveryDroppedLookupFailed: the event's targets could not be read.
	deliveryDroppedLookupFailed = "dropped: lookup failed"
)

var deliveriesDropped = func() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_event_deliveries_dropped_total",
		Help: "Bucket notifications and webhooks never attempted: the process-wide delivery queue was full (overloaded), the process was stopping (shutdown), the webhook was deleted or disabled while its job was queued (removed), or the event's targets could not be read (lookup_failed — counted once per event, whether or not the tenant or bucket had a target). A dropped webhook has a failed webhook_deliveries row while its webhook row exists (lookup_failed: when the webhook is known from the last good read).",
	}, []string{"kind", "reason"})
	for _, k := range []string{deliveryKindNotification, deliveryKindWebhook} {
		for _, r := range []string{"overloaded", "shutdown", "removed", "lookup_failed"} {
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
	tenant string
	kind   string
	db     *sql.DB
	// run delivers; ctx is the pool's (cut at the drain bound) — the job
	// sets its own deadline.
	run func(ctx context.Context)
	// owed lists the rows of a webhook job that never runs (nil for a
	// notification).
	owed func(ctx context.Context) []droppedDelivery
}

// tenantDeliveries is one tenant's FIFO and how many of its jobs run.
type tenantDeliveries struct {
	jobs    []deliveryJob
	running int
}

// deliveryPool runs jobs fairly across tenants: one FIFO per tenant (start
// order = submit order within a tenant), workers serve the tenants with
// queued jobs round-robin, at most maxRunning jobs of one tenant run at
// once and at most maxQueued wait — past that the tenant's OWN jobs are
// dropped, never another tenant's (post-merge review of d589da6: one
// tenant's blackholed webhook held every worker in 10 s POSTs and filled
// the shared queue, and every other tenant's delivery was then dropped).
type deliveryPool struct {
	workers    int
	capacity   int // queued jobs, all tenants
	maxQueued  int // queued jobs, one tenant
	maxRunning int // running jobs, one tenant
	// reserve is the part of capacity only a tenant with nothing queued may
	// take: tenants with a backlog stop at capacity-reserve, so a tenant's
	// first job is accepted even when four others fill their shares
	// (Prompt 2a.3 H2: 4 × 2,048 = the whole queue, and a fifth tenant's
	// single delivery was dropped).
	reserve int
	logger  *zap.Logger

	startOnce  sync.Once
	base       context.Context
	cancelBase context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	closed  bool
	tenants map[string]*tenantDeliveries
	ring    []string // tenants with queued jobs, served round-robin
	next    int      // the ring position served next
	queued  int

	outstanding atomic.Int64 // accepted and not finished
	accepted    atomic.Int64 // ever accepted (tests)
}

// eventDeliveries is the process's pool.
var eventDeliveries = newDeliveryPool(deliveryWorkers, deliveryQueueSize)

func newDeliveryPool(workers, queue int) *deliveryPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &deliveryPool{
		workers: workers, capacity: queue,
		maxQueued: max(queue/4, 1), maxRunning: max(workers/4, 1), reserve: queue / 8,
		base: ctx, cancelBase: cancel, tenants: map[string]*tenantDeliveries{},
	}
	p.cond = sync.NewCond(&p.mu)
	return p
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

// take blocks for the next job a worker may run (round-robin over the
// tenants under their running cap); ok is false once the pool is closed and
// nothing is queued.
func (p *deliveryPool) take() (deliveryJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		for i := 0; i < len(p.ring); i++ {
			pos := (p.next + i) % len(p.ring)
			id := p.ring[pos]
			td := p.tenants[id]
			if td.running >= p.maxRunning {
				continue
			}
			j := td.jobs[0]
			td.jobs[0] = deliveryJob{}
			td.jobs = td.jobs[1:]
			td.running++
			p.queued--
			if len(td.jobs) == 0 {
				p.ring = append(p.ring[:pos], p.ring[pos+1:]...)
				p.next = pos
			} else {
				p.next = pos + 1
			}
			if len(p.ring) > 0 {
				p.next %= len(p.ring)
			} else {
				p.next = 0
			}
			return j, true
		}
		if p.closed && p.queued == 0 {
			return deliveryJob{}, false
		}
		p.cond.Wait()
	}
}

func (p *deliveryPool) done(tenant string) {
	p.mu.Lock()
	if td := p.tenants[tenant]; td != nil {
		td.running--
		if td.running == 0 && len(td.jobs) == 0 {
			delete(p.tenants, tenant)
		}
	}
	p.mu.Unlock()
	p.cond.Broadcast() // the tenant may run another job
}

func (p *deliveryPool) work() {
	for {
		j, ok := p.take()
		if !ok {
			return
		}
		p.runJob(j)
	}
}

func (p *deliveryPool) runJob(j deliveryJob) {
	defer p.outstanding.Add(-1)
	defer p.done(j.tenant)
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
// take — the pool full (for a tenant with jobs queued: full but the
// reserve), its tenant over its share, or the process stopping — is
// recorded as dropped at once. Once one job of a tenant is dropped the
// rest of its jobs in this call are too (start order stays submit order).
func (p *deliveryPool) submit(jobs ...deliveryJob) {
	if len(jobs) == 0 {
		return
	}
	p.startWorkers()
	var overloaded, shutdown []deliveryJob
	cut := map[string]bool{}
	p.mu.Lock()
	for _, j := range jobs {
		if p.closed {
			shutdown = append(shutdown, j)
			continue
		}
		td := p.tenants[j.tenant]
		idle := td == nil || len(td.jobs) == 0
		full := p.queued >= p.capacity || (!idle && p.queued >= p.capacity-p.reserve)
		if cut[j.tenant] || full || (td != nil && len(td.jobs) >= p.maxQueued) {
			cut[j.tenant] = true
			overloaded = append(overloaded, j)
			continue
		}
		if td == nil {
			td = &tenantDeliveries{}
			p.tenants[j.tenant] = td
		}
		if len(td.jobs) == 0 {
			p.ring = append(p.ring, j.tenant)
		}
		td.jobs = append(td.jobs, j)
		p.queued++
		p.outstanding.Add(1)
		p.accepted.Add(1)
	}
	p.mu.Unlock()
	p.cond.Broadcast()
	p.recordDropped(overloaded, deliveryDroppedOverload)
	p.recordDropped(shutdown, deliveryDroppedShutdown)
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
		filtered, err := insertDroppedDeliveries(ctx, db, rs, reason)
		if err != nil {
			p.log().Error("dropped webhook deliveries not recorded", zap.Error(err), zap.Int("rows", len(rs)), zap.String("reason", reason))
		}
		if filtered > 0 {
			p.log().Warn("dropped webhook deliveries without a row: their webhook or event is gone",
				zap.Int("rows", filtered), zap.String("reason", reason))
		}
	}
}

// insertDroppedDeliveries writes the failure rows, 1,000 per statement.
// Only rows whose webhook and event still exist are inserted (a webhook
// deleted meanwhile used to fail the whole statement on its foreign key:
// 0 rows, every other tenant's included — Prompt 2a.3 H2); filtered is
// how many were left out.
func insertDroppedDeliveries(ctx context.Context, db *sql.DB, rows []droppedDelivery, reason string) (filtered int, err error) {
	for len(rows) > 0 {
		n := min(len(rows), 1000)
		ids, hooks, events := make([]string, n), make([]string, n), make([]string, n)
		for i, r := range rows[:n] {
			ids[i], hooks[i], events[i] = uuid.New().String(), r.webhookID, r.eventID
		}
		res, err := db.ExecContext(ctx, `
			INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, response_code, response_body, latency_ms)
			SELECT u.id, u.hook, u.event, 'failed', 0, $4, 0
			FROM unnest($1::text[], $2::text[], $3::text[]) AS u(id, hook, event)
			JOIN webhook_endpoints w ON w.id = u.hook
			JOIN events e ON e.id = u.event
			FOR KEY SHARE OF w, e`,
			pq.Array(ids), pq.Array(hooks), pq.Array(events), reason)
		if err != nil {
			return filtered, err
		}
		if got, err := res.RowsAffected(); err == nil {
			filtered += n - int(got)
		}
		rows = rows[n:]
	}
	return filtered, nil
}

// drain is the stop sequence's wait: up to bound for every accepted job,
// then the queued ones are recorded as dropped at shutdown and the running
// ones cut. Returns how many queued jobs were dropped. Nothing new starts
// afterwards: a later submit is recorded as dropped at once, and the
// workers exit.
func (p *deliveryPool) drain(bound time.Duration) int {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cond.Broadcast()
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
	p.mu.Lock()
	for _, id := range p.ring {
		td := p.tenants[id]
		left = append(left, td.jobs...)
		td.jobs = nil
		if td.running == 0 {
			delete(p.tenants, id)
		}
	}
	p.ring, p.next, p.queued = nil, 0, 0
	p.mu.Unlock()
	p.cond.Broadcast()
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
// budget (longBound, LONG_OP_DRAIN_BOUND's value, measured from the stop's
// start), at most deliveryDrainBound.
func (s *Server) drainDeliveries(stopStarted time.Time, longBound time.Duration) int {
	p := eventDeliveries
	p.mu.Lock()
	if p.logger == nil {
		p.logger = s.log()
	}
	p.mu.Unlock()
	bound := min(deliveryDrainBound, time.Until(stopStarted.Add(longBound)))
	return p.drain(max(bound, time.Second))
}

// webhookJob delivers one recorded event to the endpoints given, with its
// own deadline; never run, it owes a failed row per matching endpoint. gen
// is the tenant's webhook generation the endpoints were read at: when the
// webhook CRUD has moved it since, the job re-reads its endpoints' rows at
// start (Prompt 2a.3 H2: a deleted, disabled or re-pointed webhook got POSTs
// with its old URL and secret for as long as the job waited in the queue).
func webhookJob(db *sql.DB, logger *zap.Logger, endpoints []webhookEndpoint, gen uint64, eventID, eventType, tenantID string, payload []byte) deliveryJob {
	return deliveryJob{
		tenant: tenantID,
		kind:   deliveryKindWebhook,
		db:     db,
		run: func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, webhookDeliveryTimeout)
			defer cancel()
			eps := endpoints
			if webhookGeneration(tenantID) != gen {
				eps = revalidateEndpoints(ctx, db, logger, endpoints, eventID, eventType, tenantID)
			}
			deliverToEndpoints(ctx, db, logger, eps, eventID, eventType, tenantID, payload)
		},
		owed: func(context.Context) []droppedDelivery { return owedRows(endpoints, eventID, eventType) },
	}
}

// droppedRowsJob writes failed rows for deliveries that will not be
// attempted; dropped itself, it owes the same rows.
func droppedRowsJob(db *sql.DB, logger *zap.Logger, tenantID string, rows []droppedDelivery, reason string) deliveryJob {
	return deliveryJob{
		tenant: tenantID,
		kind:   deliveryKindWebhook,
		db:     db,
		run: func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, webhookDeliveryTimeout)
			defer cancel()
			if _, err := insertDroppedDeliveries(ctx, db, rows, reason); err != nil {
				logger.Error("skipped webhook deliveries not recorded", zap.Error(err), zap.Int("rows", len(rows)), zap.String("reason", reason))
			}
		},
		owed: func(context.Context) []droppedDelivery { return rows },
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

// endpointSkipped is the body of the row a webhook disabled while its job
// was queued gets (a deleted one cannot have a row: the foreign key).
const endpointSkipped = "skipped: endpoint removed"

// revalidateEndpoints re-reads the job's endpoints (one query): the current
// URL, secret and filter of each still enabled; a disabled one is recorded
// `skipped: endpoint removed`, a deleted one logged — both counted. When
// the read fails the job keeps what it captured.
func revalidateEndpoints(ctx context.Context, db *sql.DB, logger *zap.Logger, endpoints []webhookEndpoint,
	eventID, eventType, tenantID string) []webhookEndpoint {
	ids := make([]string, len(endpoints))
	for i, ep := range endpoints {
		ids[i] = ep.id
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, url, event_filter, secret, enabled FROM webhook_endpoints WHERE tenant_id = $1 AND id = ANY($2)`,
		tenantID, pq.Array(ids))
	if err != nil {
		logger.Warn("webhook endpoints not re-read before delivery; the captured ones are used", zap.Error(err), zap.String("tenant_id", tenantID))
		return endpoints
	}
	defer func() { _ = rows.Close() }()
	type current struct {
		ep      webhookEndpoint
		enabled bool
	}
	now := map[string]current{}
	for rows.Next() {
		var c current
		var filter pq.StringArray
		if err := rows.Scan(&c.ep.id, &c.ep.url, &filter, &c.ep.secret, &c.enabled); err != nil {
			logger.Warn("webhook endpoints not re-read before delivery; the captured ones are used", zap.Error(err), zap.String("tenant_id", tenantID))
			return endpoints
		}
		c.ep.filter = []string(filter)
		now[c.ep.id] = c
	}
	if err := rows.Err(); err != nil {
		logger.Warn("webhook endpoints not re-read before delivery; the captured ones are used", zap.Error(err), zap.String("tenant_id", tenantID))
		return endpoints
	}
	var out []webhookEndpoint
	for _, ep := range endpoints {
		c, ok := now[ep.id]
		switch {
		case ok && c.enabled:
			out = append(out, c.ep)
		case !matchesWebhookFilter(ep.filter, eventType):
		case ok:
			deliveriesDropped.WithLabelValues(deliveryKindWebhook, "removed").Inc()
			recordDelivery(ctx, db, logger, uuid.New().String(), ep.id, eventID, "failed", 0, endpointSkipped, 0)
		default:
			deliveriesDropped.WithLabelValues(deliveryKindWebhook, "removed").Inc()
			logger.Info("webhook deleted while its delivery was queued; not sent",
				zap.String("tenant_id", tenantID), zap.String("webhook_id", ep.id), zap.String("event_id", eventID))
		}
	}
	return out
}
