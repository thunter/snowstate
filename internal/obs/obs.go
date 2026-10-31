// Package obs provides logging, metrics and request IDs.
package obs

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
)

// NewLogger builds the process logger.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// NewRequestID returns an ID for one request.
func NewRequestID() string { return ids.V4() }

// Metrics holds every metric in SPEC §11, on a private registry.
type Metrics struct {
	Registry        *prometheus.Registry
	Requests        *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	QueryDuration   *prometheus.HistogramVec
	QueryErrors     *prometheus.CounterVec
	LockConflicts   prometheus.Counter
	StateBytes      prometheus.Summary
}

// NewMetrics creates and registers the metrics. depth reports the audit buffer.
func NewMetrics(depth func() int) *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "snowstate_http_requests_total", Help: "HTTP requests.",
		}, []string{"method", "route", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "snowstate_http_request_duration_seconds", Help: "HTTP request duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		QueryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "snowstate_snowflake_query_duration_seconds", Help: "Snowflake query duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		QueryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "snowstate_snowflake_errors_total", Help: "Snowflake errors.",
		}, []string{"op", "code"}),
		LockConflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "snowstate_lock_conflicts_total", Help: "Rejected lock or write attempts.",
		}),
		StateBytes: prometheus.NewSummary(prometheus.SummaryOpts{
			Name: "snowstate_state_bytes", Help: "Size of written states.",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		}),
	}
	m.Registry.MustRegister(m.Requests, m.RequestDuration, m.QueryDuration, m.QueryErrors,
		m.LockConflicts, m.StateBytes,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "snowstate_audit_buffer_depth", Help: "Audit rows waiting to be written.",
		}, func() float64 {
			if depth == nil {
				return 0
			}
			return float64(depth())
		}))
	return m
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ObserveRequest records one HTTP request.
func (m *Metrics) ObserveRequest(method, route string, status int, d time.Duration) {
	m.Requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.RequestDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// CodedError is implemented by errors that carry a Snowflake error number.
type CodedError interface {
	ErrorCode() int
}

// ObserveQuery implements store.Observer.
func (m *Metrics) ObserveQuery(op string, d time.Duration, err error) {
	m.QueryDuration.WithLabelValues(op).Observe(d.Seconds())
	if err == nil {
		return
	}
	code := "unknown"
	var coded CodedError
	if errors.As(err, &coded) {
		code = fmt.Sprint(coded.ErrorCode())
	}
	m.QueryErrors.WithLabelValues(op, code).Inc()
}
