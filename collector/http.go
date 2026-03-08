package collector

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTPConfig holds configuration for the HTTP probe collector.
type HTTPConfig struct {
	Interval     time.Duration    `yaml:"interval"`
	Timeout      time.Duration    `yaml:"timeout"`
	Destinations []HTTPDestination `yaml:"destinations"`
}

// HTTPDestination is a single HTTP probe target.
type HTTPDestination struct {
	Name              string `yaml:"name"`
	URL               string `yaml:"url"`
	Method            string `yaml:"method"`             // default: GET
	TLSSkipVerify     bool   `yaml:"tls_skip_verify"`    // skip TLS certificate verification
}

func (d HTTPDestination) method() string {
	if d.Method != "" {
		return d.Method
	}
	return http.MethodGet
}

type httpStats struct {
	durationSeconds float64
	statusCode      float64
	success         float64
}

// HTTPCollector probes HTTP/HTTPS destinations on a fixed interval and exposes
// response duration, status code, and success metrics.
type HTTPCollector struct {
	config  HTTPConfig
	clients map[string]*http.Client // one client per destination, keyed by name

	mu    sync.RWMutex
	stats map[string]httpStats

	durationDesc   *prometheus.Desc
	statusCodeDesc *prometheus.Desc
	successDesc    *prometheus.Desc
}

// NewHTTPCollector creates a new HTTPCollector.
func NewHTTPCollector(cfg HTTPConfig) *HTTPCollector {
	clients := make(map[string]*http.Client, len(cfg.Destinations))
	for _, dest := range cfg.Destinations {
		clients[dest.Name] = &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: dest.TLSSkipVerify, //nolint:gosec // user opt-in
				},
			},
		}
	}

	labels := []string{"destination", "url"}
	return &HTTPCollector{
		config:  cfg,
		clients: clients,
		stats:   make(map[string]httpStats, len(cfg.Destinations)),
		durationDesc: prometheus.NewDesc(
			"http_probe_duration_seconds",
			"Duration of the last HTTP probe request.",
			labels, nil,
		),
		statusCodeDesc: prometheus.NewDesc(
			"http_probe_status_code",
			"HTTP status code returned by the last probe (0 on connection error).",
			labels, nil,
		),
		successDesc: prometheus.NewDesc(
			"http_probe_success",
			"Whether the last HTTP probe succeeded (1 = 2xx response, 0 otherwise).",
			labels, nil,
		),
	}
}

// Start launches one probe loop goroutine per destination and blocks until all
// return (i.e. until ctx is cancelled).
func (c *HTTPCollector) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dest := range c.config.Destinations {
		wg.Add(1)
		go func(dest HTTPDestination) {
			defer wg.Done()
			c.runLoop(ctx, dest)
		}(dest)
	}
	wg.Wait()
}

func (c *HTTPCollector) runLoop(ctx context.Context, dest HTTPDestination) {
	c.probe(ctx, dest)

	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.probe(ctx, dest)
		}
	}
}

func (c *HTTPCollector) probe(ctx context.Context, dest HTTPDestination) {
	req, err := http.NewRequestWithContext(ctx, dest.method(), dest.URL, nil)
	if err != nil {
		slog.Warn("failed to build HTTP probe request", "destination", dest.Name, "url", dest.URL, "error", err)
		c.store(dest.Name, httpStats{})
		return
	}

	start := time.Now()
	resp, err := c.clients[dest.Name].Do(req)
	duration := time.Since(start).Seconds()

	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn("HTTP probe error", "destination", dest.Name, "url", dest.URL, "error", err)
		c.store(dest.Name, httpStats{durationSeconds: duration})
		return
	}
	// Drain and discard body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	success := 0.0
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		success = 1
	}
	c.store(dest.Name, httpStats{
		durationSeconds: duration,
		statusCode:      float64(resp.StatusCode),
		success:         success,
	})
}

func (c *HTTPCollector) store(name string, s httpStats) {
	c.mu.Lock()
	c.stats[name] = s
	c.mu.Unlock()
}

// Describe implements prometheus.Collector.
func (c *HTTPCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.durationDesc
	ch <- c.statusCodeDesc
	ch <- c.successDesc
}

// Collect implements prometheus.Collector.
func (c *HTTPCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snapshot := make(map[string]httpStats, len(c.stats))
	for k, v := range c.stats {
		snapshot[k] = v
	}
	c.mu.RUnlock()

	for _, dest := range c.config.Destinations {
		s, ok := snapshot[dest.Name]
		if !ok {
			continue
		}
		lv := []string{dest.Name, dest.URL}
		ch <- prometheus.MustNewConstMetric(c.durationDesc, prometheus.GaugeValue, s.durationSeconds, lv...)
		ch <- prometheus.MustNewConstMetric(c.statusCodeDesc, prometheus.GaugeValue, s.statusCode, lv...)
		ch <- prometheus.MustNewConstMetric(c.successDesc, prometheus.GaugeValue, s.success, lv...)
	}
}
