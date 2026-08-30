package pgq

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rustwizard/cleargo/jobq"
)

// queueMetrics aggregates Prometheus metrics for all pgq queues that share
// one registry. Queues are told apart by the "table" label, and depth gauges
// are refreshed on every scrape by querying each queue's Stats.
type queueMetrics struct {
	opsTotal     *prometheus.CounterVec // labels: table, op
	jobsByStatus *prometheus.GaugeVec   // labels: table, status

	mu     sync.Mutex
	queues []*Postgres
}

// registryMetrics maps a prometheus.Registerer to its shared queueMetrics.
var registryMetrics sync.Map // key: prometheus.Registerer, value: *queueMetrics

// registerQueueMetrics returns the shared metric set for reg, creating and
// registering it on first use, and attaches q to it.
func registerQueueMetrics(reg prometheus.Registerer, q *Postgres) (*queueMetrics, error) {
	if existing, ok := registryMetrics.Load(reg); ok {
		m := existing.(*queueMetrics)
		m.addQueue(q)
		return m, nil
	}

	m := &queueMetrics{
		opsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "jobq",
			Name:      "ops_total",
			Help:      "Total number of queue operations by table and operation.",
		}, []string{"table", "op"}),
		jobsByStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "jobq",
			Name:      "jobs_by_status",
			Help:      "Current number of jobs in the queue by table and status.",
		}, []string{"table", "status"}),
	}
	if err := reg.Register(m); err != nil {
		return nil, fmt.Errorf("register collector: %w", err)
	}
	m.addQueue(q)
	registryMetrics.Store(reg, m)
	return m, nil
}

func (m *queueMetrics) addQueue(q *Postgres) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues = append(m.queues, q)
}

// Describe implements prometheus.Collector.
func (m *queueMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.opsTotal.Describe(ch)
	m.jobsByStatus.Describe(ch)
}

// Collect implements prometheus.Collector. Depth gauges are refreshed from
// the database; a failing queue is skipped rather than breaking the scrape.
func (m *queueMetrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.Lock()
	queues := append([]*Postgres(nil), m.queues...)
	m.mu.Unlock()

	for _, q := range queues {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stats, err := q.Stats(ctx)
		cancel()
		if err != nil {
			continue // keep last scraped values on transient errors
		}

		statusCounts := map[jobq.Status]int{
			jobq.Pending:    stats.Pending,
			jobq.Processing: stats.Processing,
			jobq.Done:       stats.Done,
			jobq.Failed:     stats.Failed,
		}
		for status, n := range statusCounts {
			ch <- prometheus.MustNewConstMetric(
				m.jobsByStatus.WithLabelValues(q.table, string(status)).Desc(),
				prometheus.GaugeValue,
				float64(n),
				q.table, string(status),
			)
		}
	}

	m.opsTotal.Collect(ch)
}
