package collector

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DNSQueryType controls which DNS record type is queried.
type DNSQueryType string

const (
	DNSQueryTypeA    DNSQueryType = "A"
	DNSQueryTypeAAAA DNSQueryType = "AAAA"
)

// DNSConfig holds configuration for the DNS probe collector.
type DNSConfig struct {
	Interval     time.Duration    `yaml:"interval" validate:"gt=0"`
	Timeout      time.Duration    `yaml:"timeout" validate:"gt=0"`
	Destinations []DNSDestination `yaml:"destinations" validate:"dive"`
}

// DNSDestination is a single DNS probe target.
type DNSDestination struct {
	Name      string       `yaml:"name" validate:"required"`
	Server    string       `yaml:"server" validate:"required,ip"`     // nameserver IP address (e.g. "8.8.8.8", "[2001:4860:4860::8888]")
	Query     string       `yaml:"query" validate:"required,hostname"` // domain name to resolve (e.g. "google.com")
	QueryType DNSQueryType `yaml:"query_type" validate:"omitempty,oneof=A AAAA"` // A | AAAA (default: A)
}

// resolvedQueryType returns the query type, defaulting to A.
func (d DNSDestination) resolvedQueryType() DNSQueryType {
	if d.QueryType == DNSQueryTypeAAAA {
		return DNSQueryTypeAAAA
	}
	return DNSQueryTypeA
}

// network returns the net.Resolver network string for this query type.
func (d DNSDestination) network() string {
	if d.resolvedQueryType() == DNSQueryTypeAAAA {
		return "ip6"
	}
	return "ip4"
}

// dialNetwork returns the UDP dial network for reaching the nameserver.
func (d DNSDestination) dialNetwork() string {
	ip := net.ParseIP(d.Server)
	if ip != nil && ip.To4() == nil {
		return "udp6"
	}
	return "udp4"
}

// serverAddr returns the nameserver address with port.
func (d DNSDestination) serverAddr() string {
	_, _, err := net.SplitHostPort(d.Server)
	if err != nil {
		return net.JoinHostPort(d.Server, "53")
	}
	return d.Server
}

type dnsStatsKey string // destination name

type dnsStats struct {
	durationSeconds float64
	success         float64
}

// DNSCollector probes DNS servers on a fixed interval and exposes query
// duration and success metrics. It uses the standard library net.Resolver
// with a custom nameserver to perform A or AAAA lookups.
type DNSCollector struct {
	config    DNSConfig
	resolvers map[dnsStatsKey]*net.Resolver

	mu    sync.RWMutex
	stats map[dnsStatsKey]dnsStats

	durationDesc *prometheus.Desc
	successDesc  *prometheus.Desc
}

// NewDNSCollector creates a new DNSCollector.
func NewDNSCollector(cfg DNSConfig) *DNSCollector {
	resolvers := make(map[dnsStatsKey]*net.Resolver, len(cfg.Destinations))
	for _, dest := range cfg.Destinations {
		dest := dest // capture for closure
		resolvers[dnsStatsKey(dest.Name)] = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: cfg.Timeout}
				return d.DialContext(ctx, dest.dialNetwork(), dest.serverAddr())
			},
		}
	}

	labels := []string{"destination", "server", "query", "query_type"}
	return &DNSCollector{
		config:    cfg,
		resolvers: resolvers,
		stats:     make(map[dnsStatsKey]dnsStats),
		durationDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "dns_probe", "duration_seconds"),
			"Duration of the last DNS probe query.",
			labels, nil,
		),
		successDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "dns_probe", "success"),
			"Whether the last DNS probe succeeded (1 = got answer, 0 otherwise).",
			labels, nil,
		),
	}
}

// Start launches one probe loop goroutine per destination and blocks until
// all return (i.e. until ctx is cancelled).
func (c *DNSCollector) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dest := range c.config.Destinations {
		wg.Add(1)
		go func(dest DNSDestination) {
			defer wg.Done()
			c.runLoop(ctx, dest)
		}(dest)
	}
	wg.Wait()
}

func (c *DNSCollector) runLoop(ctx context.Context, dest DNSDestination) {
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

func (c *DNSCollector) probe(ctx context.Context, dest DNSDestination) {
	key := dnsStatsKey(dest.Name)
	qt := dest.resolvedQueryType()
	log := slog.With("destination", dest.Name, "server", dest.Server, "query", dest.Query, "query_type", qt)

	resolver := c.resolvers[key]

	queryCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	log.Debug("probing DNS destination")

	start := time.Now()
	ips, err := resolver.LookupNetIP(queryCtx, dest.network(), dest.Query)
	duration := time.Since(start).Seconds()

	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("DNS probe error", "error", err)
		c.store(key, dnsStats{durationSeconds: duration})
		return
	}

	success := 0.0
	if len(ips) > 0 {
		success = 1
	}

	log.Debug("DNS probe complete", "answers", len(ips), "duration", duration, "success", success == 1)
	c.store(key, dnsStats{
		durationSeconds: duration,
		success:         success,
	})
}

func (c *DNSCollector) store(key dnsStatsKey, s dnsStats) {
	c.mu.Lock()
	c.stats[key] = s
	c.mu.Unlock()
}

// Describe implements prometheus.Collector.
func (c *DNSCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.durationDesc
	ch <- c.successDesc
}

// Collect implements prometheus.Collector.
func (c *DNSCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snapshot := make(map[dnsStatsKey]dnsStats, len(c.stats))
	for k, v := range c.stats {
		snapshot[k] = v
	}
	c.mu.RUnlock()

	for _, dest := range c.config.Destinations {
		s, ok := snapshot[dnsStatsKey(dest.Name)]
		if !ok {
			continue
		}
		lv := []string{dest.Name, dest.Server, dest.Query, string(dest.resolvedQueryType())}
		ch <- prometheus.MustNewConstMetric(c.durationDesc, prometheus.GaugeValue, s.durationSeconds, lv...)
		ch <- prometheus.MustNewConstMetric(c.successDesc, prometheus.GaugeValue, s.success, lv...)
	}
}
