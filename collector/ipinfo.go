package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// IPInfoConfig holds configuration for the IPInfo collector.
type IPInfoConfig struct {
	Token           string        `yaml:"token"`
	RefreshInterval time.Duration `yaml:"refresh_interval" validate:"gt=0"`
}

type ipInfoResponse struct {
	IP            string `json:"ip"`
	ASN           string `json:"asn"`
	ASName        string `json:"as_name"`
	ASDomain      string `json:"as_domain"`
	CountryCode   string `json:"country_code"`
	Country       string `json:"country"`
	ContinentCode string `json:"continent_code"`
	Continent     string `json:"continent"`
}

// IPInfoCollector fetches public IP information from ipinfo.io for both IPv4
// and IPv6 and exposes it as a Prometheus info metric.
type IPInfoCollector struct {
	config     IPInfoConfig
	httpClient *http.Client

	mu       sync.RWMutex
	ipv4Info *ipInfoResponse
	ipv6Info *ipInfoResponse

	infoDesc *prometheus.Desc
}

// NewIPInfoCollector creates a new IPInfoCollector.
func NewIPInfoCollector(cfg IPInfoConfig) *IPInfoCollector {
	return &IPInfoCollector{
		config:     cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		infoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "info"),
			"Public IP address information from ipinfo.io lite API. Value is the AS number.",
			[]string{"version", "ip", "asn", "as_name", "as_domain", "country_code", "country", "continent_code", "continent"},
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
	slog.Debug("refreshing public IP info")

	type result struct {
		info *ipInfoResponse
		err  error
	}

	v4ch := make(chan result, 1)
	v6ch := make(chan result, 1)

	go func() {
		info, err := c.fetch(ctx, "https://v4.api.ipinfo.io/lite/me")
		v4ch <- result{info, err}
	}()
	go func() {
		info, err := c.fetch(ctx, "https://v6.api.ipinfo.io/lite/me")
		v6ch <- result{info, err}
	}()

	v4 := <-v4ch
	v6 := <-v6ch

	if v4.err != nil {
		slog.Warn("failed to fetch IPv4 public IP info", "error", v4.err)
	} else {
		slog.Debug("fetched IPv4 public IP info", "ip", v4.info.IP, "asn", v4.info.ASN, "country", v4.info.Country)
	}
	if v6.err != nil {
		slog.Warn("failed to fetch IPv6 public IP info", "error", v6.err)
	} else {
		slog.Debug("fetched IPv6 public IP info", "ip", v6.info.IP, "asn", v6.info.ASN, "country", v6.info.Country)
	}

	c.mu.Lock()
	if v4.info != nil {
		c.ipv4Info = v4.info
	}
	if v6.info != nil {
		c.ipv6Info = v6.info
	}
	c.mu.Unlock()
}

// token returns the API token, preferring the IPINFO_TOKEN environment variable
// over the value set in config, so a Kubernetes secret can be injected without
// embedding credentials in the ConfigMap.
func (c *IPInfoCollector) token() string {
	if t := os.Getenv("IPINFO_TOKEN"); t != "" {
		return t
	}
	return c.config.Token
}

func (c *IPInfoCollector) fetch(ctx context.Context, base string) (*ipInfoResponse, error) {
	client := c.httpClient
	url := base
	if t := c.token(); t != "" {
		url = fmt.Sprintf("%s?token=%s", base, t)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	slog.Debug("fetching ipinfo", "url", base)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	slog.Debug("ipinfo response", "url", base, "status", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from ipinfo.io", resp.StatusCode)
	}

	var info ipInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &info, nil
}

// parseASNumber extracts the numeric AS number from a string like "AS15169"
// and returns it as a float64. Returns 0 if parsing fails.
func parseASNumber(asn string) float64 {
	n, err := strconv.ParseFloat(strings.TrimPrefix(asn, "AS"), 64)
	if err != nil {
		return 0
	}
	return n
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
			c.infoDesc, prometheus.GaugeValue, parseASNumber(ipv4Info.ASN),
			"4", ipv4Info.IP, ipv4Info.ASN, ipv4Info.ASName, ipv4Info.ASDomain,
			ipv4Info.CountryCode, ipv4Info.Country, ipv4Info.ContinentCode, ipv4Info.Continent,
		)
	}
	if ipv6Info != nil {
		ch <- prometheus.MustNewConstMetric(
			c.infoDesc, prometheus.GaugeValue, parseASNumber(ipv6Info.ASN),
			"6", ipv6Info.IP, ipv6Info.ASN, ipv6Info.ASName, ipv6Info.ASDomain,
			ipv6Info.CountryCode, ipv6Info.Country, ipv6Info.ContinentCode, ipv6Info.Continent,
		)
	}
}
