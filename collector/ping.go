package collector

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	probing "github.com/prometheus-community/pro-bing"
	"github.com/prometheus/client_golang/prometheus"
)

// AddressFamily controls which IP version is used for pinging.
type AddressFamily string

const (
	AddressFamilyIPv4 AddressFamily = "ipv4"
	AddressFamilyIPv6 AddressFamily = "ipv6"
	AddressFamilyBoth AddressFamily = "both"
)

// PingConfig holds configuration for the ping collector.
type PingConfig struct {
	Interval     time.Duration `yaml:"interval"`
	Count        int           `yaml:"count"`
	Timeout      time.Duration `yaml:"timeout"`
	Privileged   bool          `yaml:"privileged"`
	Destinations []Destination `yaml:"destinations"`
}

// Destination is a single ping target.
type Destination struct {
	Name          string        `yaml:"name"`
	Host          string        `yaml:"host"`
	AddressFamily AddressFamily `yaml:"address_family"` // ipv4 | ipv6 | both (default: both)
}

// families returns the address families this destination should probe.
// When address_family is unset and the host is a literal IP address, the
// family is inferred automatically instead of defaulting to both.
func (d Destination) families() []AddressFamily {
	switch d.AddressFamily {
	case AddressFamilyIPv4:
		return []AddressFamily{AddressFamilyIPv4}
	case AddressFamilyIPv6:
		return []AddressFamily{AddressFamilyIPv6}
	case AddressFamilyBoth:
		return []AddressFamily{AddressFamilyIPv4, AddressFamilyIPv6}
	default: // unset — infer from literal IP, else probe both
		if ip := net.ParseIP(d.Host); ip != nil {
			if ip.To4() != nil {
				return []AddressFamily{AddressFamilyIPv4}
			}
			return []AddressFamily{AddressFamilyIPv6}
		}
		return []AddressFamily{AddressFamilyIPv4, AddressFamilyIPv6}
	}
}

type statsKey struct {
	name string
	af   AddressFamily
}

type pingStats struct {
	avgRTT    float64
	minRTT    float64
	maxRTT    float64
	lossRatio float64
}

// PingCollector sends ICMP pings to configured destinations on a fixed interval
// and exposes RTT and packet-loss metrics labelled by address family.
type PingCollector struct {
	config PingConfig

	mu    sync.RWMutex
	stats map[statsKey]pingStats

	rttAvgDesc *prometheus.Desc
	rttMinDesc *prometheus.Desc
	rttMaxDesc *prometheus.Desc
	lossDesc   *prometheus.Desc
}

// NewPingCollector creates a new PingCollector.
func NewPingCollector(cfg PingConfig) *PingCollector {
	labels := []string{"destination", "host", "version"}
	return &PingCollector{
		config: cfg,
		stats:  make(map[statsKey]pingStats),
		rttAvgDesc: prometheus.NewDesc(
			"ping_rtt_avg_seconds",
			"Average round-trip time of ICMP pings.",
			labels, nil,
		),
		rttMinDesc: prometheus.NewDesc(
			"ping_rtt_min_seconds",
			"Minimum round-trip time of ICMP pings.",
			labels, nil,
		),
		rttMaxDesc: prometheus.NewDesc(
			"ping_rtt_max_seconds",
			"Maximum round-trip time of ICMP pings.",
			labels, nil,
		),
		lossDesc: prometheus.NewDesc(
			"ping_loss_ratio",
			"Packet loss ratio for ICMP pings (0 = no loss, 1 = total loss).",
			labels, nil,
		),
	}
}

// Start launches one goroutine per (destination, address-family) pair and
// blocks until all return (i.e. until ctx is cancelled).
func (c *PingCollector) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dest := range c.config.Destinations {
		for _, af := range dest.families() {
			wg.Add(1)
			go func(dest Destination, af AddressFamily) {
				defer wg.Done()
				c.runLoop(ctx, dest, af)
			}(dest, af)
		}
	}
	wg.Wait()
}

func (c *PingCollector) runLoop(ctx context.Context, dest Destination, af AddressFamily) {
	// Ping immediately, then on every tick.
	c.ping(ctx, dest, af)

	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.ping(ctx, dest, af)
		}
	}
}

// afNetwork converts an AddressFamily to the network string expected by pro-bing.
func afNetwork(af AddressFamily) string {
	if af == AddressFamilyIPv6 {
		return "ip6"
	}
	return "ip4"
}

// afVersion returns the version label value for the address family.
func afVersion(af AddressFamily) string {
	if af == AddressFamilyIPv6 {
		return "6"
	}
	return "4"
}

func (c *PingCollector) ping(ctx context.Context, dest Destination, af AddressFamily) {
	key := statsKey{name: dest.Name, af: af}
	log := slog.With("destination", dest.Name, "host", dest.Host, "version", afVersion(af))

	pinger, err := probing.NewPinger(dest.Host)
	if err != nil {
		log.Warn("failed to create pinger", "error", err)
		c.store(key, pingStats{lossRatio: 1})
		return
	}

	pinger.Count = c.config.Count
	pinger.Timeout = c.config.Timeout
	pinger.SetPrivileged(c.config.Privileged)
	pinger.SetNetwork(afNetwork(af))

	// Stop the pinger if the context is cancelled before it finishes.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			pinger.Stop()
		case <-done:
		}
	}()

	log.Debug("starting ping", "count", c.config.Count, "timeout", c.config.Timeout, "network", afNetwork(af))

	if err := pinger.Run(); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("ping error", "error", err)
		c.store(key, pingStats{lossRatio: 1})
		return
	}

	s := pinger.Statistics()
	log.Debug("ping complete", "avg_rtt", s.AvgRtt, "min_rtt", s.MinRtt, "max_rtt", s.MaxRtt, "loss_pct", s.PacketLoss)
	c.store(key, pingStats{
		avgRTT:    s.AvgRtt.Seconds(),
		minRTT:    s.MinRtt.Seconds(),
		maxRTT:    s.MaxRtt.Seconds(),
		lossRatio: s.PacketLoss / 100.0,
	})
}

func (c *PingCollector) store(key statsKey, s pingStats) {
	c.mu.Lock()
	c.stats[key] = s
	c.mu.Unlock()
}

// Describe implements prometheus.Collector.
func (c *PingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.rttAvgDesc
	ch <- c.rttMinDesc
	ch <- c.rttMaxDesc
	ch <- c.lossDesc
}

// Collect implements prometheus.Collector.
func (c *PingCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snapshot := make(map[statsKey]pingStats, len(c.stats))
	for k, v := range c.stats {
		snapshot[k] = v
	}
	c.mu.RUnlock()

	for _, dest := range c.config.Destinations {
		for _, af := range dest.families() {
			s, ok := snapshot[statsKey{name: dest.Name, af: af}]
			if !ok {
				continue
			}
			lv := []string{dest.Name, dest.Host, afVersion(af)}
			ch <- prometheus.MustNewConstMetric(c.rttAvgDesc, prometheus.GaugeValue, s.avgRTT, lv...)
			ch <- prometheus.MustNewConstMetric(c.rttMinDesc, prometheus.GaugeValue, s.minRTT, lv...)
			ch <- prometheus.MustNewConstMetric(c.rttMaxDesc, prometheus.GaugeValue, s.maxRTT, lv...)
			ch <- prometheus.MustNewConstMetric(c.lossDesc, prometheus.GaugeValue, s.lossRatio, lv...)
		}
	}
}
