package collector

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTPConfig holds configuration for the HTTP probe collector.
type HTTPConfig struct {
	Interval     time.Duration     `yaml:"interval" validate:"gt=0"`
	Timeout      time.Duration     `yaml:"timeout" validate:"gt=0"`
	Destinations []HTTPDestination `yaml:"destinations" validate:"dive"`
}

// HTTPDestination is a single HTTP probe target.
type HTTPDestination struct {
	Name          string        `yaml:"name" validate:"required"`
	URL           string        `yaml:"url" validate:"required,http_url"`
	Method        string        `yaml:"method" validate:"omitempty,oneof=GET POST PUT DELETE HEAD OPTIONS PATCH"` // default: GET
	TLSSkipVerify bool          `yaml:"tls_skip_verify"`                                                         // skip TLS certificate verification
	AddressFamily AddressFamily `yaml:"address_family" validate:"omitempty,oneof=ipv4 ipv6 both"`                // ipv4 | ipv6 | both (default: both)
	DNSProtocol   DNSProtocol   `yaml:"dns_protocol" validate:"omitempty,oneof=udp tcp"`                         // udp | tcp (default: udp)
}

func (d HTTPDestination) method() string {
	if d.Method != "" {
		return d.Method
	}
	return http.MethodGet
}

// families returns the address families this destination should probe.
func (d HTTPDestination) families() []AddressFamily {
	return d.AddressFamily.Families()
}

type httpStatsKey struct {
	name string
	af   AddressFamily
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
	clients map[httpStatsKey]*http.Client

	mu    sync.RWMutex
	stats map[httpStatsKey]httpStats

	durationDesc   *prometheus.Desc
	statusCodeDesc *prometheus.Desc
	successDesc    *prometheus.Desc
}

// NewHTTPCollector creates a new HTTPCollector.
func NewHTTPCollector(cfg HTTPConfig) *HTTPCollector {
	clients := make(map[httpStatsKey]*http.Client)
	for _, dest := range cfg.Destinations {
		for _, af := range dest.families() {
			key := httpStatsKey{name: dest.Name, af: af}
			network := "tcp4"
			if af == AddressFamilyIPv6 {
				network = "tcp6"
			}
			d := &net.Dialer{Timeout: cfg.Timeout}
			if dest.DNSProtocol == DNSProtocolTCP {
				d.Resolver = &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, dnsNetwork, address string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, strings.Replace(dnsNetwork, "udp", "tcp", 1), address)
					},
				}
			}
			clients[key] = &http.Client{
				Timeout: cfg.Timeout,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
						return d.DialContext(ctx, network, addr)
					},
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: dest.TLSSkipVerify, //nolint:gosec // user opt-in
					},
				},
			}
		}
	}

	labels := []string{"destination", "url", "version"}
	return &HTTPCollector{
		config:  cfg,
		clients: clients,
		stats:   make(map[httpStatsKey]httpStats),
		durationDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "http_probe", "duration_seconds"),
			"Duration of the last HTTP probe request.",
			labels, nil,
		),
		statusCodeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "http_probe", "status_code"),
			"HTTP status code returned by the last probe (0 on connection error).",
			labels, nil,
		),
		successDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "http_probe", "success"),
			"Whether the last HTTP probe succeeded (1 = 2xx response, 0 otherwise).",
			labels, nil,
		),
	}
}

// Start launches one probe loop goroutine per (destination, address-family)
// pair and blocks until all return (i.e. until ctx is cancelled).
func (c *HTTPCollector) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dest := range c.config.Destinations {
		for _, af := range dest.families() {
			wg.Add(1)
			go func(dest HTTPDestination, af AddressFamily) {
				defer wg.Done()
				c.runLoop(ctx, dest, af)
			}(dest, af)
		}
	}
	wg.Wait()
}

func (c *HTTPCollector) runLoop(ctx context.Context, dest HTTPDestination, af AddressFamily) {
	c.probe(ctx, dest, af)

	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.probe(ctx, dest, af)
		}
	}
}

func (c *HTTPCollector) probe(ctx context.Context, dest HTTPDestination, af AddressFamily) {
	key := httpStatsKey{name: dest.Name, af: af}
	version := afVersion(af)

	req, err := http.NewRequestWithContext(ctx, dest.method(), dest.URL, nil)
	if err != nil {
		slog.Warn("failed to build HTTP probe request", "destination", dest.Name, "url", dest.URL, "version", version, "error", err)
		c.store(key, httpStats{})
		return
	}

	slog.Debug("probing HTTP destination", "destination", dest.Name, "url", dest.URL, "method", dest.method(), "version", version)

	start := time.Now()
	resp, err := c.clients[key].Do(req)
	duration := time.Since(start).Seconds()

	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn("HTTP probe error", "destination", dest.Name, "url", dest.URL, "version", version, "error", err)
		c.store(key, httpStats{durationSeconds: duration})
		return
	}
	defer resp.Body.Close()
	// Drain up to 1 MB of the body so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	success := 0.0
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		success = 1
	}
	slog.Debug("HTTP probe complete", "destination", dest.Name, "version", version, "status", resp.StatusCode, "duration", duration, "success", success == 1)
	c.store(key, httpStats{
		durationSeconds: duration,
		statusCode:      float64(resp.StatusCode),
		success:         success,
	})
}

func (c *HTTPCollector) store(key httpStatsKey, s httpStats) {
	c.mu.Lock()
	c.stats[key] = s
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
	snapshot := make(map[httpStatsKey]httpStats, len(c.stats))
	for k, v := range c.stats {
		snapshot[k] = v
	}
	c.mu.RUnlock()

	for _, dest := range c.config.Destinations {
		for _, af := range dest.families() {
			s, ok := snapshot[httpStatsKey{name: dest.Name, af: af}]
			if !ok {
				continue
			}
			lv := []string{dest.Name, dest.URL, afVersion(af)}
			ch <- prometheus.MustNewConstMetric(c.durationDesc, prometheus.GaugeValue, s.durationSeconds, lv...)
			ch <- prometheus.MustNewConstMetric(c.statusCodeDesc, prometheus.GaugeValue, s.statusCode, lv...)
			ch <- prometheus.MustNewConstMetric(c.successDesc, prometheus.GaugeValue, s.success, lv...)
		}
	}
}
