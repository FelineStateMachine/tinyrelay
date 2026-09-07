package daemon

import (
	"context"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestQueueCollectorBoundsKindsAndAccumulatesScrapeFailures(t *testing.T) {
	store, err := storage.Open(context.Background(), t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{tenants: map[string]*Tenant{"one": {store: store}}}
	collector := app.QueueCollector()
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(collector); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES('id','unbounded','event','target','{}',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if got := metricValue(families, "relay_work_pending", "kind", "other"); got != 1 {
		t.Fatalf("other pending = %v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	first := gatherMetric(t, registry.Gather, "relay_work_collection_errors_total")
	second := gatherMetric(t, registry.Gather, "relay_work_collection_errors_total")
	if first < 1 || second <= first {
		t.Fatalf("scrape errors are not cumulative: first=%v second=%v", first, second)
	}
}

func metricValue(families []*dto.MetricFamily, name, labelName, labelValue string) float64 {
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

func gatherMetric(t *testing.T, gather func() ([]*dto.MetricFamily, error), name string) float64 {
	t.Helper()
	families, err := gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) > 0 {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}
