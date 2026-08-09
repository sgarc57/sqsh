// Package observability provides structured logging and the minimal metric
// set of this slice (SPEC.md §15). Each later change adds the metrics for the
// subsystem it introduces.
package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DebugOutputLimit bounds command output written to debug logs.
const DebugOutputLimit = 4096

// NewLogger returns a JSON slog logger at the named level.
func NewLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

// Metrics is the minimal sqshd metric set.
type Metrics struct {
	Registry *prometheus.Registry

	MessagesReceived     prometheus.Counter
	Commands             *prometheus.CounterVec   // command, status
	Rejections           *prometheus.CounterVec   // reason
	CommandDuration      *prometheus.HistogramVec // command
	AWSErrors            *prometheus.CounterVec   // api
	LastSuccessfulPoll   prometheus.Gauge
	ResultWriteFailures  *prometheus.CounterVec // op
	DowngradesPrevented  prometheus.Counter
	VisibilityExtensions *prometheus.CounterVec // command
	InFlight             prometheus.Gauge
}

// NewMetrics registers the metric set on a fresh registry.
func NewMetrics() *Metrics {
	r := prometheus.NewRegistry()
	f := func(c prometheus.Collector) { r.MustRegister(c) }
	m := &Metrics{
		Registry: r,
		MessagesReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sqshd_messages_received_total", Help: "Messages received from the queue."}),
		Commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqshd_commands_total", Help: "Requests reaching a terminal status, by command and status."},
			[]string{"command", "status"}),
		Rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqshd_rejections_total", Help: "Rejected requests by reason."}, []string{"reason"}),
		CommandDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sqshd_command_duration_seconds", Help: "Execution time of commands.",
			Buckets: prometheus.ExponentialBuckets(0.01, 4, 8)}, []string{"command"}),
		AWSErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqshd_aws_errors_total", Help: "AWS API errors by API."}, []string{"api"}),
		LastSuccessfulPoll: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sqshd_last_successful_poll_timestamp", Help: "Unix time of the last successful receive."}),
		ResultWriteFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqshd_result_write_failures_total", Help: "Result store writes that failed, by op."}, []string{"op"}),
		DowngradesPrevented: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sqshd_result_downgrades_prevented_total", Help: "Result writes dropped by status monotonicity."}),
		VisibilityExtensions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqshd_visibility_extensions_total", Help: "Visibility extensions for running commands."}, []string{"command"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sqshd_commands_in_flight", Help: "Messages currently being handled."}),
	}
	f(m.MessagesReceived)
	f(m.Commands)
	f(m.Rejections)
	f(m.CommandDuration)
	f(m.AWSErrors)
	f(m.LastSuccessfulPoll)
	f(m.ResultWriteFailures)
	f(m.DowngradesPrevented)
	f(m.VisibilityExtensions)
	f(m.InFlight)
	return m
}

// Serve exposes the registry on addr until ctx is done. It returns once the
// listener is bound, so a bad address fails startup.
func (m *Metrics) Serve(ctx context.Context, addr string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped", "err", err)
		}
	}()
	return nil
}
