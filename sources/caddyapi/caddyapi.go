// Package caddyapi provides a Source that discovers hostnames from a running
// Caddy server through its admin API.
//
// It reads the live configuration with GET /config/ and collects the values of
// every "host" matcher, including matchers nested in subroutes (Caddyfile
// "handle" blocks). Because Caddy adapts Caddyfiles to the same JSON model,
// this works for Caddyfile and JSON configurations alike.
//
// Unlike the label-based "caddy" source, this source does not read workloads:
// Extract is a no-op and all hostnames come from Discover.
package caddyapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/maxfield-allison/dnsweaver/pkg/httputil"
	"github.com/maxfield-allison/dnsweaver/pkg/source"
	"github.com/maxfield-allison/dnsweaver/pkg/workload"
)

const sourceName = "caddy-api"

// requestTimeout bounds a single GET against the Caddy admin API.
const requestTimeout = 10 * time.Second

// CaddyAPI implements source.Source by reading the Caddy admin API.
type CaddyAPI struct {
	configURL string
	http      *http.Client
	logger    *slog.Logger
}

// Option is a functional option for configuring CaddyAPI.
type Option func(*CaddyAPI)

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(c *CaddyAPI) {
		c.logger = logger
	}
}

// New creates a Caddy admin API source for the given base URL
// (e.g. "http://localhost:2019").
func New(rawURL string, opts ...Option) (*CaddyAPI, error) {
	base, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	c := &CaddyAPI{
		configURL: base.String() + "/config/",
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	c.http = httputil.NewClient(&httputil.ClientConfig{
		Timeout: requestTimeout,
		Logger:  c.logger,
	})
	return c, nil
}

// ParseURL validates a Caddy admin API base URL.
//
// Only http and https URLs with a host are accepted. Any path, query,
// fragment, or userinfo is rejected because the source always requests
// "/config/" itself.
func ParseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid caddy admin URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid caddy admin URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid caddy admin URL %q: missing host", raw)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("invalid caddy admin URL %q: must be a bare base URL such as http://localhost:2019", raw)
	}
	u.Path = ""
	return u, nil
}

// Name returns the source identifier.
func (c *CaddyAPI) Name() string {
	return sourceName
}

// Extract is a no-op: this source does not read workload labels.
func (c *CaddyAPI) Extract(_ context.Context, _ workload.Workload) ([]source.Hostname, error) {
	return nil, nil
}

// Discover reads the running Caddy configuration and returns the hostnames in
// its host matchers.
//
// An unreachable or failing admin API is reported as an error so the
// reconciler treats the cycle as incomplete and does not delete records for
// hostnames it could not see.
func (c *CaddyAPI) Discover(ctx context.Context) ([]source.Hostname, error) {
	cfg, err := c.fetchConfig(ctx)
	if err != nil {
		return nil, err
	}

	hostnames := extractHostnames(cfg, c.logger)
	c.logger.Debug("discovered hostnames from caddy admin API",
		slog.Int("count", len(hostnames)),
	)
	return hostnames, nil
}

// SupportsDiscovery always returns true: discovery is this source's only mode.
func (c *CaddyAPI) SupportsDiscovery() bool {
	return true
}

// SupportedPlatforms returns the platforms this source handles. An empty
// slice means "any"; Extract ignores workloads regardless.
func (c *CaddyAPI) SupportedPlatforms() []workload.Platform {
	return nil
}

// caddyConfig is the subset of Caddy's JSON config that carries hostnames.
type caddyConfig struct {
	Apps struct {
		HTTP struct {
			Servers map[string]caddyServer `json:"servers"`
		} `json:"http"`
	} `json:"apps"`
}

type caddyServer struct {
	Routes []caddyRoute `json:"routes"`
}

type caddyRoute struct {
	// Match is a list of matcher sets; each set maps matcher name to its
	// config. Only the "host" matcher is decoded.
	Match []map[string]json.RawMessage `json:"match"`
	// Handle is decoded leniently so that subroute handlers can be walked.
	Handle []caddyHandler `json:"handle"`
}

type caddyHandler struct {
	Handler string       `json:"handler"`
	Routes  []caddyRoute `json:"routes"`
}

// fetchConfig GETs /config/ and decodes the HTTP app's servers.
func (c *CaddyAPI) fetchConfig(ctx context.Context) (*caddyConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.configURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building caddy admin request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying caddy admin API: %w", err)
	}
	defer resp.Body.Close()

	body, err := httputil.ReadBody(resp, 0)
	if err != nil {
		return nil, fmt.Errorf("reading caddy admin response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caddy admin API returned %s: %s", resp.Status, strings.TrimSpace(truncate(string(body), 200)))
	}

	var cfg caddyConfig
	// A Caddy instance with no config at all returns the JSON literal null,
	// which decodes to the zero value: zero servers, zero hostnames.
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("decoding caddy config: %w", err)
	}
	return &cfg, nil
}

// extractHostnames returns every concrete hostname in the config's host
// matchers, walking nested subroutes. Wildcards, placeholders, and IP
// addresses are skipped because they cannot become DNS host records.
// Results are ordered by server name, then route order, without duplicates.
func extractHostnames(cfg *caddyConfig, logger *slog.Logger) []source.Hostname {
	serverNames := make([]string, 0, len(cfg.Apps.HTTP.Servers))
	for name := range cfg.Apps.HTTP.Servers {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	seen := make(map[string]struct{})
	var out []source.Hostname

	var walk func(server string, routes []caddyRoute)
	walk = func(server string, routes []caddyRoute) {
		for _, route := range routes {
			for _, set := range route.Match {
				raw, ok := set["host"]
				if !ok {
					continue
				}
				var hosts []string
				if err := json.Unmarshal(raw, &hosts); err != nil {
					logger.Debug("skipping unparseable caddy host matcher",
						slog.String("server", server),
						slog.String("error", err.Error()),
					)
					continue
				}
				for _, h := range hosts {
					name, ok := normalizeHost(h)
					if !ok {
						logger.Debug("skipping non-concrete caddy host",
							slog.String("server", server),
							slog.String("host", h),
						)
						continue
					}
					if _, dup := seen[name]; dup {
						continue
					}
					seen[name] = struct{}{}
					out = append(out, source.Hostname{
						Name:   name,
						Source: sourceName,
						Router: server,
					})
				}
			}
			for _, h := range route.Handle {
				if len(h.Routes) > 0 {
					walk(server, h.Routes)
				}
			}
		}
	}

	for _, name := range serverNames {
		walk(name, cfg.Apps.HTTP.Servers[name].Routes)
	}
	return out
}

// normalizeHost lowercases a host matcher value and reports whether it names
// a single concrete DNS hostname.
func normalizeHost(h string) (string, bool) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" || strings.ContainsAny(h, "*{}") {
		return "", false
	}
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return "", false
	}
	return h, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
