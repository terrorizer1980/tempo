package spanmetrics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	semconv "go.opentelemetry.io/collector/model/semconv/v1.5.0"

	gen "github.com/grafana/tempo/modules/generator/processor"
	"github.com/grafana/tempo/pkg/tempopb"
	v1_resource "github.com/grafana/tempo/pkg/tempopb/resource/v1"
	v1_trace "github.com/grafana/tempo/pkg/tempopb/trace/v1"
)

const (
	name          = "spanmetrics"
	callsMetric   = "calls_total"
	latencyCount  = "latency_count"
	latencySum    = "latency_sum"
	latencyBucket = "latency_bucket"
)

var (
	metricActiveSeries = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempo",
		Name:      "metrics_processor_span_metrics_active_series",
		Help:      "The amount of series currently active",
	}, []string{"tenant"})
)

type processor struct {
	namespace string

	// TODO: possibly split mutex into two: one for the metrics and one for the cache.
	//  cache's mutex should be RWMutex.
	mtx sync.Mutex
	// TODO: need a mechanism to clean up inactive series,
	//  otherwise this is unbounded memory usage.
	calls               map[string]float64
	latencyCount        map[string]float64
	latencySum          map[string]float64
	latencyBucketCounts map[string][]float64
	latencyBuckets      []float64
	cache               map[string]labels.Labels

	// Crude mechanism to track if a key was recently updated. For every key store a counter
	// - when a key is updated, reset the counter to 0
	// - at every collect increase the value, if the value is above a threshold it's stale
	// TODO is it even worth keeping track of 'staleness', why not just clear all the maps after a
	//  collect?
	stalenessCounter map[string]int

	metricActiveSeries prometheus.Gauge
}

func New(tenant string) gen.Processor {
	return &processor{
		namespace:           "tempo",
		calls:               make(map[string]float64),
		latencyCount:        make(map[string]float64),
		latencySum:          make(map[string]float64),
		latencyBucketCounts: make(map[string][]float64),
		// TODO: make this configurable.
		latencyBuckets: []float64{1, 10, 50, 100, 500},
		cache:          make(map[string]labels.Labels),

		stalenessCounter: make(map[string]int),

		metricActiveSeries: metricActiveSeries.WithLabelValues(tenant),
	}
}

func (p *processor) Name() string { return name }

func (p *processor) PushSpans(ctx context.Context, req *tempopb.PushSpansRequest) error {
	p.aggregateMetrics(req.Batches)

	return nil
}

func (p *processor) Shutdown(context.Context) error { return nil }

func (p *processor) aggregateMetrics(resourceSpans []*v1_trace.ResourceSpans) {
	for _, rs := range resourceSpans {
		svcName := getServiceName(rs.Resource)
		if svcName == "" {
			continue
		}
		for _, ils := range rs.InstrumentationLibrarySpans {
			for _, span := range ils.Spans {
				p.aggregateMetricsForSpan(svcName, span)
			}
		}
	}
}

func (p *processor) aggregateMetricsForSpan(svcName string, span *v1_trace.Span) {
	key := p.buildKey(svcName, span)

	latencyMS := float64(span.GetEndTimeUnixNano()-span.GetStartTimeUnixNano()) / float64(time.Millisecond.Nanoseconds())

	p.mtx.Lock()
	p.cacheLabels(key, svcName, span)
	p.stalenessCounter[key] = 0
	p.calls[key]++
	p.aggregateLatencyMetrics(key, latencyMS)
	p.mtx.Unlock()
}

func (p *processor) aggregateLatencyMetrics(key string, latencyMS float64) {
	// TODO: make this configurable
	if _, ok := p.latencyBucketCounts[key]; !ok {
		p.latencyBucketCounts[key] = make([]float64, len(p.latencyBuckets)+1)
	}

	p.latencyCount[key]++
	p.latencySum[key] += latencyMS
	idx := sort.SearchFloat64s(p.latencyBuckets, latencyMS)
	for i := 0; i < idx; i++ {
		p.latencyBucketCounts[key][i]++
	}
}

func (p *processor) CollectMetrics(ctx context.Context, appender storage.Appender) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "spanmetrics.CollectMetrics")
	defer span.Finish()

	p.mtx.Lock()
	defer p.mtx.Unlock()

	// increment stateleness counters and filter out stale keys
	for key := range p.stalenessCounter {
		p.stalenessCounter[key]++
		// hasn't been updated for 4 collects (i.e. 1 minute), remove it
		if p.stalenessCounter[key] >= 4 {
			delete(p.stalenessCounter, key)
			delete(p.calls, key)
			delete(p.latencyCount, key)
			delete(p.latencySum, key)
			delete(p.latencyBucketCounts, key)
			delete(p.cache, key)
		}
	}

	p.metricActiveSeries.Set(float64(len(p.calls)))

	timestampMs := time.Now().UnixMilli()

	if err := p.collectCalls(appender, timestampMs); err != nil {
		return err
	}

	if err := p.collectLatencyMetrics(appender, timestampMs); err != nil {
		return err
	}

	return nil
}

func (p *processor) collectCalls(appender storage.Appender, timestampMs int64) error {
	// TODO: only collect new data points.
	for key, count := range p.calls {
		lbls := p.getLabels(key, callsMetric)

		if _, err := appender.Append(0, lbls, timestampMs, count); err != nil {
			return err
		}
	}
	return nil
}

func (p *processor) collectLatencyMetrics(appender storage.Appender, timestampMs int64) error {
	// TODO: only collect new data points.
	for key := range p.latencyCount {
		// Collect latency count
		lbls := p.getLabels(key, latencyCount)
		if _, err := appender.Append(0, lbls, timestampMs, p.latencyCount[key]); err != nil {
			return err
		}

		// Collect latency sum
		lbls = p.getLabels(key, latencySum)
		if _, err := appender.Append(0, lbls, timestampMs, p.latencySum[key]); err != nil {
			return err
		}

		// Collect latency buckets
		for i, count := range p.latencyBucketCounts[key] {
			if i == len(p.latencyBuckets) {
				lbls = append(p.getLabels(key, latencyBucket), labels.Label{Name: "le", Value: "+Inf"})
			} else {
				lbls = append(p.getLabels(key, latencyBucket), labels.Label{Name: "le", Value: strconv.Itoa(int(p.latencyBuckets[i]))})
			}
			if _, err := appender.Append(0, lbls, timestampMs, count); err != nil {
				return err
			}
		}

	}
	return nil
}

func (p *processor) buildKey(svcName string, span *v1_trace.Span) string {
	// TODO: add more dimensions
	key := fmt.Sprintf("%s_%s_%s_%s", svcName, span.Name, span.Kind, span.Status)

	return key
}

// Must be called under lock
func (p *processor) cacheLabels(key string, svcName string, span *v1_trace.Span) {
	p.cache[key] = labels.Labels{
		{Name: "service", Value: svcName},
		{Name: "span_name", Value: span.Name},
		{Name: "span_kind", Value: span.Kind.String()},
		{Name: "span_status", Value: span.Status.Code.String()},
	}
}

// Must be called under lock
func (p *processor) getLabels(key, metricName string) labels.Labels {
	// TODO: check if it doesn't exist?
	lbls := p.cache[key]

	lbls = append(lbls, labels.Label{Name: "__name__", Value: fmt.Sprintf("%s_%s", p.namespace, metricName)})

	return lbls
}

func getServiceName(rs *v1_resource.Resource) string {
	for _, attr := range rs.Attributes {
		if attr.Key == semconv.AttributeServiceName {
			return attr.Value.GetStringValue()
		}
	}

	return ""
}
