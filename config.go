package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"publicip-exporter/collector"
)

type ServerConfig struct {
	ListenAddress string `yaml:"listen_address"`
}

type Config struct {
	Server ServerConfig           `yaml:"server"`
	IPInfo collector.IPInfoConfig  `yaml:"ipinfo"`
	Ping   collector.PingConfig    `yaml:"ping"`
	HTTP   collector.HTTPConfig    `yaml:"http"`
	DNS    collector.DNSConfig     `yaml:"dns"`
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

	return cfg, nil
}
