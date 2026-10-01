package obs

import (
	"net/http"
	"regexp"
	"runtime"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rootxkit/uspace-core/core"
)

// Namespace prefixes every metric of this system.
const Namespace = "ussp"

// Version and Commit are set at link time:
//
//	-ldflags "-X github.com/rootxkit/uspace-ussp/internal/obs.Version=v0.1.0
//	          -X github.com/rootxkit/uspace-ussp/internal/obs.Commit=<sha>"
var (
	Version = "dev"
	Commit  = "unknown"
)

// NewRegistry returns a registry with the Go runtime and process
// collectors and ussp_build_info{process,version,commit,go_version} = 1.
func NewRegistry(process string) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "build_info",
		Help:      "Build of this process; always 1.",
		ConstLabels: prometheus.Labels{
			"process": process, "version": Version, "commit": Commit, "go_version": runtime.Version(),
		},
	})
	build.Set(1)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		build,
	)
	return reg
}

// MetricsHandler serves reg in the Prometheus text format.
func MetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

// CountersCollector exposes a core.Counters as gauges named
// ussp_<counter> with a constant "component" label (E-09: everything
// refused, dropped or degraded is counted with a stable snake_case
// name). It is unchecked: names appear as they are first incremented.
type CountersCollector struct {
	component string
	counters  *core.Counters
}

// NewCountersCollector returns a collector over counters.
func NewCountersCollector(component string, counters *core.Counters) *CountersCollector {
	return &CountersCollector{component: component, counters: counters}
}

// Describe sends nothing: the collector is unchecked.
func (c *CountersCollector) Describe(chan<- *prometheus.Desc) {}

// Collect sends one gauge per counter.
func (c *CountersCollector) Collect(ch chan<- prometheus.Metric) {
	for name, v := range c.counters.Snapshot() {
		desc := prometheus.NewDesc(MetricName(name), "core.Counters value "+name+" (E-09).",
			nil, prometheus.Labels{"component": c.component})
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(v))
	}
}

var invalidMetricChars = regexp.MustCompile(`[^a-z0-9_]`)

// MetricName is the Prometheus name of a counter; a character
// Prometheus does not accept becomes "_", so a malformed name is still
// visible rather than dropped.
func MetricName(counter string) string {
	return Namespace + "_" + invalidMetricChars.ReplaceAllString(strings.ToLower(counter), "_")
}
