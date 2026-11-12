package engine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	mCycles = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_cycles_total", Help: "Capture cycles completed.",
	})
	mCycleSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "docsync_cycle_seconds", Help: "Duration of capture cycles.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	})
	mChanges = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_source_changes_total", Help: "Change tracking rows captured.",
	})
	mRoots = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_roots_assembled_total", Help: "Documents assembled (including unchanged ones).",
	})
	mUpserts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_documents_upserted_total", Help: "Documents emitted as upserts.",
	})
	mDeletes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_documents_deleted_total", Help: "Documents emitted as deletes.",
	})
	mRootErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_root_errors_total", Help: "Documents that failed to assemble.",
	})
	mSweepCorrections = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_sweep_corrections_total", Help: "Stale documents found and fixed by the sweeper (missed changes).",
	})
	mErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docsync_errors_total", Help: "Engine errors (cycles, sinks, housekeeping).",
	})
	mTableChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "docsync_table_changes_total", Help: "Change tracking rows captured per table.",
	}, []string{"table"})
	mTableFanout = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "docsync_table_fanout_roots", Help: "Roots affected by one cycle's changes to a table.",
		Buckets: prometheus.ExponentialBuckets(1, 4, 12),
	}, []string{"table"})
)

// Registry holds docsync's metrics; the dashboard serves it at /metrics.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(mCycles, mCycleSeconds, mChanges, mRoots, mUpserts, mDeletes,
		mRootErrors, mSweepCorrections, mErrors, mTableChanges, mTableFanout,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
}

func cycleMetrics(r CycleResult) {
	mCycles.Inc()
	mCycleSeconds.Observe(r.Duration.Seconds())
	mChanges.Add(float64(r.Changes))
	mRoots.Add(float64(r.Roots))
	mUpserts.Add(float64(r.Upserts))
	mDeletes.Add(float64(r.Deletes))
	mRootErrors.Add(float64(r.Errors))
	mSweepCorrections.Add(float64(r.SweepCorrections))
}

func tableMetrics(table string, changes, roots int) {
	mTableChanges.WithLabelValues(table).Add(float64(changes))
	mTableFanout.WithLabelValues(table).Observe(float64(roots))
}

func errorMetric() { mErrors.Inc() }

// gaugeCollector reports point-in-time values read from the engine on each scrape.
type gaugeCollector struct {
	e                                          *Engine
	gen, pending, lag, sinceCycle, sinceChange *prometheus.Desc
	paused                                     *prometheus.Desc
}

// NewGaugeCollector exposes checkpoint, pending-set and sink lag gauges for e.
func NewGaugeCollector(e *Engine) prometheus.Collector {
	return &gaugeCollector{
		e:           e,
		gen:         prometheus.NewDesc("docsync_generation", "Latest committed generation.", nil, nil),
		pending:     prometheus.NewDesc("docsync_pending_roots", "Roots waiting in the pending set.", nil, nil),
		lag:         prometheus.NewDesc("docsync_sink_lag_generations", "Generations committed but not yet written by a sink.", []string{"sink"}, nil),
		sinceCycle:  prometheus.NewDesc("docsync_seconds_since_last_cycle", "Seconds since the last cycle finished.", nil, nil),
		sinceChange: prometheus.NewDesc("docsync_seconds_since_last_change", "Seconds since a cycle last captured changes.", nil, nil),
		paused:      prometheus.NewDesc("docsync_paused", "1 when the pipeline is paused.", nil, nil),
	}
}

func (g *gaugeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- g.gen
	ch <- g.pending
	ch <- g.lag
	ch <- g.sinceCycle
	ch <- g.sinceChange
	ch <- g.paused
}

func (g *gaugeCollector) Collect(ch chan<- prometheus.Metric) {
	v := g.e.Status()
	ch <- prometheus.MustNewConstMetric(g.gen, prometheus.GaugeValue, float64(v.Checkpoint.Generation))
	ch <- prometheus.MustNewConstMetric(g.pending, prometheus.GaugeValue, float64(v.Pending))
	for _, s := range v.Sinks {
		ch <- prometheus.MustNewConstMetric(g.lag, prometheus.GaugeValue, float64(s.Lag), s.Name)
	}
	if !v.LastCycleAt.IsZero() {
		ch <- prometheus.MustNewConstMetric(g.sinceCycle, prometheus.GaugeValue, time.Since(v.LastCycleAt).Seconds())
	}
	if !v.LastChangeAt.IsZero() {
		ch <- prometheus.MustNewConstMetric(g.sinceChange, prometheus.GaugeValue, time.Since(v.LastChangeAt).Seconds())
	}
	paused := 0.0
	if v.Paused {
		paused = 1
	}
	ch <- prometheus.MustNewConstMetric(g.paused, prometheus.GaugeValue, paused)
}
