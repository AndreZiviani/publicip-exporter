package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// IPInfoConfig holds configuration for the IPInfo collector.
type IPInfoConfig struct {
	Token           string        `yaml:"token"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

type ipInfoResponse struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Org      string `json:"org"`
	Timezone string `json:"timezone"`
}

// IPInfoCollector fetches public IP information from ipinfo.io for both IPv4
// and IPv6 and exposes it as a Prometheus info metric.
type IPInfoCollector struct {
	config     IPInfoConfig
	ipv4Client *http.Client
	ipv6Client *http.Client

	mu       sync.RWMutex
	ipv4Info *ipInfoResponse
	ipv6Info *ipInfoResponse

	infoDesc *prometheus.Desc
}

// NewIPInfoCollector creates a new IPInfoCollector.
func NewIPInfoCollector(cfg IPInfoConfig) *IPInfoCollector {
	// IPv4 client forces tcp4 so it always hits the IPv4 endpoint.
	d := &net.Dialer{Timeout: 10 * time.Second}
	ipv4Client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, "tcp4", addr)
			},
		},
	}
	// IPv6 client uses the v6.ipinfo.io hostname which resolves to an IPv6-only
	// address, so no custom dialer is needed.
	ipv6Client := &http.Client{Timeout: 15 * time.Second}

	return &IPInfoCollector{
		config:     cfg,
		ipv4Client: ipv4Client,
		ipv6Client: ipv6Client,
		infoDesc: prometheus.NewDesc(
			"publicip_info",
			"Public IP address information from ipinfo.io. Value is always 1.",
			[]string{"version", "ip", "hostname", "city", "region", "country", "org", "timezone"},
			nil,
		),
	}
}

// Start runs the background refresh loop until ctx is cancelled.
func (c *IPInfoCollector) Start(ctx context.Context) {
	c.refresh(ctx)

	ticker := time.NewTicker(c.config.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *IPInfoCollector) refresh(ctx context.Context) {
	ipv4, err := c.fetch(ctx, c.ipv4Client, "https://ipinfo.io/json")
	if err != nil {
		slog.Warn("failed to fetch IPv4 public IP info", "error", err)
	}

	ipv6, err := c.fetch(ctx, c.ipv6Client, "https://v6.ipinfo.io/json")
	if err != nil {
		slog.Warn("failed to fetch IPv6 public IP info", "error", err)
	}

	c.mu.Lock()
	if ipv4 != nil {
		c.ipv4Info = ipv4
	}
	if ipv6 != nil {
		c.ipv6Info = ipv6
	}
	c.mu.Unlock()
}

func (c *IPInfoCollector) fetch(ctx context.Context, client *http.Client, base string) (*ipInfoResponse, error) {
	url := base
	if c.config.Token != "" {
		url = fmt.Sprintf("%s?token=%s", base, c.config.Token)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from ipinfo.io", resp.StatusCode)
	}

	var info ipInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &info, nil
}

// Describe implements prometheus.Collector.
func (c *IPInfoCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.infoDesc
}

// Collect implements prometheus.Collector.
func (c *IPInfoCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	ipv4Info := c.ipv4Info
	ipv6Info := c.ipv6Info
	c.mu.RUnlock()

	if ipv4Info != nil {
		ch <- prometheus.MustNewConstMetric(
			c.infoDesc, prometheus.GaugeValue, 1,
			"4", ipv4Info.IP, ipv4Info.Hostname, ipv4Info.City,
			ipv4Info.Region, ipv4Info.Country, ipv4Info.Org, ipv4Info.Timezone,
		)
	}
	if ipv6Info != nil {
		ch <- prometheus.MustNewConstMetric(
			c.infoDesc, prometheus.GaugeValue, 1,
			"6", ipv6Info.IP, ipv6Info.Hostname, ipv6Info.City,
			ipv6Info.Region, ipv6Info.Country, ipv6Info.Org, ipv6Info.Timezone,
		)
	}
}
