package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"

	"publicip-exporter/collector"
)

type ServerConfig struct {
	ListenAddress string `yaml:"listen_address" validate:"required"`
}

type Config struct {
	Server       ServerConfig           `yaml:"server"`
	GlobalLabels map[string]string      `yaml:"global_labels"`
	IPInfo       collector.IPInfoConfig `yaml:"ipinfo"`
	Ping         collector.PingConfig   `yaml:"ping"`
	HTTP         collector.HTTPConfig   `yaml:"http"`
	DNS          collector.DNSConfig    `yaml:"dns"`
}

func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			ListenAddress: ":9101",
		},
		IPInfo: collector.IPInfoConfig{
			RefreshInterval: 60 * time.Second,
		},
		Ping: collector.PingConfig{
			Interval:   time.Second,
			Count:      3,
			Timeout:    5 * time.Second,
			Privileged: false,
		},
		HTTP: collector.HTTPConfig{
			Interval: 30 * time.Second,
			Timeout:  10 * time.Second,
		},
		DNS: collector.DNSConfig{
			Interval: 30 * time.Second,
			Timeout:  5 * time.Second,
		},
	}
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}

	validate := validator.New(validator.WithRequiredStructEnabled())
	if err := validate.Struct(cfg); err != nil {
		return nil, fmt.Errorf("validating config %q: %w", path, err)
	}

	if err := validateSemantics(cfg); err != nil {
		return nil, fmt.Errorf("validating config %q: %w", path, err)
	}

	logConfig(cfg)

	return cfg, nil
}

// collectorSection describes one collector's timing and destination names for validation/logging.
type collectorSection struct {
	name     string
	timeout  time.Duration
	interval time.Duration
	destNames []string
}

func collectorsFromConfig(cfg *Config) []collectorSection {
	return []collectorSection{
		{"ping", cfg.Ping.Timeout, cfg.Ping.Interval, destNames(cfg.Ping.Destinations, func(d collector.Destination) string { return d.Name })},
		{"http", cfg.HTTP.Timeout, cfg.HTTP.Interval, destNames(cfg.HTTP.Destinations, func(d collector.HTTPDestination) string { return d.Name })},
		{"dns", cfg.DNS.Timeout, cfg.DNS.Interval, destNames(cfg.DNS.Destinations, func(d collector.DNSDestination) string { return d.Name })},
	}
}

func destNames[T any](dests []T, name func(T) string) []string {
	names := make([]string, len(dests))
	for i, d := range dests {
		names[i] = name(d)
	}
	return names
}

// validateSemantics checks cross-field constraints that struct tags cannot express.
func validateSemantics(cfg *Config) error {
	for _, s := range collectorsFromConfig(cfg) {
		if len(s.destNames) > 0 && s.timeout >= s.interval {
			return fmt.Errorf("%s: timeout (%s) must be less than interval (%s)", s.name, s.timeout, s.interval)
		}
		seen := make(map[string]struct{}, len(s.destNames))
		for _, n := range s.destNames {
			if _, ok := seen[n]; ok {
				return fmt.Errorf("%s: duplicate destination name %q", s.name, n)
			}
			seen[n] = struct{}{}
		}
	}
	return nil
}

// logConfig logs a summary of the active configuration at startup.
func logConfig(cfg *Config) {
	if len(cfg.GlobalLabels) > 0 {
		slog.Info("global labels configured", "count", len(cfg.GlobalLabels))
	}
	slog.Info("ipinfo collector enabled", "refresh_interval", cfg.IPInfo.RefreshInterval.String())
	for _, s := range collectorsFromConfig(cfg) {
		if len(s.destNames) == 0 {
			slog.Warn(s.name + " collector has no destinations configured")
		} else {
			slog.Info(s.name+" collector enabled", "destinations", len(s.destNames), "interval", s.interval.String())
		}
	}
}
