package daemon

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var queueMetricKinds = []string{"delivery", "push", "replication", "webhook", "job", "records-projection", "site-mirror", "git-metadata", "catalog-owner", "callback", "delivery-discovery", "notification-delivery", "notification-broadcast", "notification", "other"}

type queueCollector struct {
	app      *App
	pending  *prometheus.Desc
	errors   *prometheus.Desc
	failures atomic.Uint64
}

// QueueCollector returns one process-wide collector aggregating all currently
// loaded tenants without exposing tenant identifiers as metric labels.
func (a *App) QueueCollector() prometheus.Collector {
	return &queueCollector{
		app:     a,
		pending: prometheus.NewDesc("relay_work_pending", "Pending durable work intents across loaded tenants.", []string{"kind"}, nil),
		errors:  prometheus.NewDesc("relay_work_collection_errors_total", "Failed durable work queue tenant scrapes.", nil, nil),
	}
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.errors
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	counts := make(map[string]float64, len(queueMetricKinds))
	for _, kind := range queueMetricKinds {
		counts[kind] = 0
	}
	loaded := c.tenants()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var failures uint64
	for _, tenant := range loaded {
		rows, err := tenant.store.DB().QueryContext(ctx, `SELECT kind,count(*) FROM work_intents WHERE state='pending' GROUP BY kind`)
		if err != nil {
			failures++
			continue
		}
		for rows.Next() {
			var kind string
			var count float64
			if err := rows.Scan(&kind, &count); err != nil {
				failures++
				break
			}
			counts[queueKind(kind)] += count
		}
		if err := rows.Err(); err != nil {
			failures++
		}
		_ = rows.Close()
	}
	c.failures.Add(failures)
	for _, kind := range queueMetricKinds {
		ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, counts[kind], kind)
	}
	ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, float64(c.failures.Load()))
}

func (c *queueCollector) tenants() []*Tenant {
	c.app.mu.Lock()
	defer c.app.mu.Unlock()
	loaded := make([]*Tenant, 0, len(c.app.tenants))
	for _, tenant := range c.app.tenants {
		loaded = append(loaded, tenant)
	}
	return loaded
}

func queueKind(kind string) string {
	for _, known := range queueMetricKinds[:len(queueMetricKinds)-1] {
		if kind == known {
			return known
		}
	}
	return "other"
}

var _ prometheus.Collector = (*queueCollector)(nil)
