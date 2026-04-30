# publicip-exporter

A Prometheus exporter for monitoring public IP information and network connectivity. It provides metrics for public IP geolocation, ICMP ping latency, HTTP probe status, and DNS resolution — with full IPv4/IPv6 dual-stack support.

## Features

- **Public IP Info** — Fetches geolocation data from [ipinfo.io](https://ipinfo.io) for both IPv4 and IPv6, exposing AS number, city, country, org, and more as metric labels.
- **Ping Probes** — Measures RTT (avg/min/max) and packet loss to configurable destinations using ICMP or unprivileged UDP pings.
- **HTTP Probes** — Monitors HTTP/HTTPS endpoints with response time, status code, and success metrics. Supports custom methods and TLS verification skip.
- **DNS Probes** — Checks DNS resolution against custom nameservers with duration and success metrics. Supports A and AAAA query types.
- **Dual-Stack** — Per-destination control over address family (`ipv4`, `ipv6`, or `both`). Metrics are labeled with IP version.
- **Unprivileged Mode** — Ping works without root via UDP-based probes (no `CAP_NET_RAW` required).
- **Config Validation** — Comprehensive validation with clear error messages on startup.
- **Multi-Architecture** — Docker images for `amd64` and `arm64`, published to Docker Hub and GHCR.

## Metrics

All metrics use the `publicip_` namespace.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `publicip_info` | Gauge | `version`, `ip`, `hostname`, `city`, `region`, `country`, `org`, `timezone` | Public IP info (value = AS number) |
| `publicip_ping_rtt_avg_seconds` | Gauge | `destination`, `host`, `version` | Average ping RTT |
| `publicip_ping_rtt_min_seconds` | Gauge | `destination`, `host`, `version` | Minimum ping RTT |
| `publicip_ping_rtt_max_seconds` | Gauge | `destination`, `host`, `version` | Maximum ping RTT |
| `publicip_ping_loss_ratio` | Gauge | `destination`, `host`, `version` | Packet loss ratio (0–1) |
| `publicip_http_probe_duration_seconds` | Gauge | `destination`, `url`, `version` | HTTP response time |
| `publicip_http_probe_status_code` | Gauge | `destination`, `url`, `version` | HTTP status code |
| `publicip_http_probe_success` | Gauge | `destination`, `url`, `version` | 1 if 2xx, 0 otherwise |
| `publicip_dns_probe_duration_seconds` | Gauge | `destination`, `server`, `query`, `query_type` | DNS lookup duration |
| `publicip_dns_probe_success` | Gauge | `destination`, `server`, `query`, `query_type` | 1 if resolved, 0 otherwise |

Standard Go runtime and process metrics are also exposed.

## Quick Start

### Binary

```bash
# Download from GitHub Releases, then:
./publicip-exporter
# or with a custom config:
./publicip-exporter -config config.yaml
```

### Docker

```bash
docker run -p 9101:9101 -v ./config.yaml:/config.yaml ghcr.io/andreziviani/publicip-exporter
```

Metrics are available at `http://localhost:9101/metrics`.

## Configuration

Configuration is loaded from `config.yaml` in the working directory. If no file is found, sensible defaults are used.

```yaml
server:
  listen_address: ":9101"

ipinfo:
  # Optional API token from ipinfo.io for higher rate limits.
  # token: "your_token_here"
  refresh_interval: 60s

http:
  interval: 30s
  timeout: 10s
  destinations:
    - name: google
      url: https://google.com
    - name: cloudflare
      url: https://cloudflare.com
    # - name: internal-api
    #   url: https://internal.example.com/health
    #   method: GET
    #   tls_skip_verify: true
    #   dns_protocol: udp       # udp | tcp (default: udp)

ping:
  interval: 5s
  count: 3
  timeout: 5s
  # Set to true if running as root or with CAP_NET_RAW (raw ICMP).
  # Set to false to use unprivileged UDP-based pings (works without root on most systems).
  privileged: false
  destinations:
    - name: google-dns
      host: 8.8.8.8
      address_family: ipv4   # ipv4 | ipv6 | both (default: both)
    - name: cloudflare-dns
      host: 1.1.1.1
      address_family: ipv4
    - name: google
      host: google.com
      # address_family omitted -> defaults to both (probes IPv4 and IPv6)
    - name: cloudflare
      host: cloudflare.com

dns:
  interval: 30s
  timeout: 5s
  destinations:
    - name: google-dns
      server: 8.8.8.8
      query: google.com
      query_type: A             # A | AAAA (default: A)
      protocol: udp             # udp | tcp (default: udp)
    - name: cloudflare-dns
      server: 1.1.1.1
      query: cloudflare.com
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `IPINFO_TOKEN` | ipinfo.io API token (overrides config file). Useful for Kubernetes secret injection. |
| `LOG_LEVEL` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) |

## Endpoints

| Path | Description |
|------|-------------|
| `/metrics` | Prometheus metrics (OpenMetrics format) |
| `/healthz` | Health check (always 200 OK) |
| `/` | Landing page |

## Building

```bash
go build -o publicip-exporter .
```

### Release

Releases are automated via [GoReleaser](https://goreleaser.com/) and GitHub Actions. Push a tag to trigger a release:

```bash
git tag v1.0.0
git push --tags
```

This builds binaries for Linux/macOS (amd64/arm64) and publishes multi-arch Docker images to:
- `docker.io/andreziviani/publicip-exporter`
- `ghcr.io/andreziviani/publicip-exporter`

## License

See [LICENSE](LICENSE) for details.
