package caddyapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/maxfield-allison/dnsweaver/pkg/source"
	"github.com/maxfield-allison/dnsweaver/pkg/workload"
)

// adaptedCaddyfile is the shape `caddy adapt` produces for:
//
//	app.example.com, WWW.example.com { reverse_proxy app:80 }
//	*.example.com {
//	    @git host git.example.com
//	    handle @git { reverse_proxy gitea:3000 }
//	}
//	:8080 { respond "metrics" }
//
// plus a second server with an IP host, a placeholder host, a duplicate,
// and a host inside a "not" matcher.
const adaptedCaddyfile = `{
  "admin": {"listen": "localhost:2019"},
  "apps": {
    "http": {
      "servers": {
        "srv1": {
          "listen": [":9443"],
          "routes": [
            {"match": [{"host": ["192.0.2.5", "{env.SITE}", "app.example.com"]}]},
            {"match": [{"not": [{"host": ["hidden.example.com"]}]}]},
            {"match": [{"host": ["api.example.org."]}]}
          ]
        },
        "srv0": {
          "listen": [":443"],
          "routes": [
            {
              "match": [{"host": ["app.example.com", "WWW.example.com"]}],
              "handle": [{"handler": "subroute", "routes": [
                {"handle": [{"handler": "reverse_proxy", "upstreams": [{"dial": "app:80"}]}]}
              ]}],
              "terminal": true
            },
            {
              "match": [{"host": ["*.example.com"]}],
              "handle": [{"handler": "subroute", "routes": [
                {
                  "group": "group0",
                  "match": [{"host": ["git.example.com"]}],
                  "handle": [{"handler": "subroute", "routes": [
                    {"handle": [{"handler": "reverse_proxy", "upstreams": [{"dial": "gitea:3000"}]}]}
                  ]}]
                }
              ]}],
              "terminal": true
            }
          ]
        },
        "srv2": {
          "listen": [":8080"],
          "routes": [{"handle": [{"handler": "static_response", "body": "metrics"}]}]
        }
      }
    }
  }
}`

func newAdminServer(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/config/" {
			t.Errorf("path = %q, want /config/", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newAdminSource(t *testing.T, rawURL string) *CaddyAPI {
	t.Helper()
	c, err := New(rawURL, WithLogger(testLogger()))
	if err != nil {
		t.Fatalf("New(%q): %v", rawURL, err)
	}
	return c
}

func TestCaddyAPI_ImplementsSource(t *testing.T) {
	var _ source.Source = (*CaddyAPI)(nil)
}

func TestCaddyAPI_NameAndModes(t *testing.T) {
	c := newAdminSource(t, "http://localhost:2019")
	if c.Name() != "caddy-api" {
		t.Errorf("Name() = %q, want caddy-api", c.Name())
	}
	if !c.SupportsDiscovery() {
		t.Error("SupportsDiscovery() = false, want true")
	}
	hs, err := c.Extract(context.Background(), workload.Workload{Labels: map[string]string{"caddy": "app.example.com"}})
	if err != nil || hs != nil {
		t.Errorf("Extract() = %v, %v; want nil, nil", hs, err)
	}
}

func TestNew_InvalidURL(t *testing.T) {
	if _, err := New("localhost:2019"); err == nil {
		t.Error("New() error = nil, want error for URL without scheme")
	}
}

func TestCaddyAPI_Discover_ExtractsHostnames(t *testing.T) {
	srv, calls := newAdminServer(t, http.StatusOK, adaptedCaddyfile)
	c := newAdminSource(t, srv.URL)

	hs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if *calls != 1 {
		t.Errorf("admin API calls = %d, want 1", *calls)
	}

	type got struct{ name, router string }
	var gotHosts []got
	for _, h := range hs {
		if h.Source != "caddy-api" {
			t.Errorf("hostname %q source = %q, want caddy-api", h.Name, h.Source)
		}
		gotHosts = append(gotHosts, got{h.Name, h.Router})
	}
	want := []got{
		{"app.example.com", "srv0"},
		{"www.example.com", "srv0"},
		{"git.example.com", "srv0"},
		{"api.example.org", "srv1"},
	}
	if !reflect.DeepEqual(gotHosts, want) {
		t.Errorf("Discover() hostnames = %v, want %v", gotHosts, want)
	}
}

func TestCaddyAPI_Discover_EmptyConfig(t *testing.T) {
	for _, body := range []string{"null", "{}", `{"apps":{"tls":{}}}`} {
		t.Run(body, func(t *testing.T) {
			srv, _ := newAdminServer(t, http.StatusOK, body)
			hs, err := newAdminSource(t, srv.URL).Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			if len(hs) != 0 {
				t.Errorf("Discover() = %v, want none", hs)
			}
		})
	}
}

// Failures must surface as errors so the reconciler marks the cycle
// incomplete instead of deleting records for hostnames it could not see.
func TestCaddyAPI_Discover_FailuresAreErrors(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		srv, _ := newAdminServer(t, http.StatusForbidden, `{"error":"host not allowed"}`)
		if _, err := newAdminSource(t, srv.URL).Discover(context.Background()); err == nil {
			t.Fatal("Discover() error = nil, want error for 403")
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		srv, _ := newAdminServer(t, http.StatusOK, `{"apps":`)
		if _, err := newAdminSource(t, srv.URL).Discover(context.Background()); err == nil {
			t.Fatal("Discover() error = nil, want decode error")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv, _ := newAdminServer(t, http.StatusOK, "{}")
		c := newAdminSource(t, srv.URL)
		srv.Close()
		if _, err := c.Discover(context.Background()); err == nil {
			t.Fatal("Discover() error = nil, want connection error")
		}
	})
}

func TestParseURL(t *testing.T) {
	valid := map[string]string{
		"http://localhost:2019":   "http://localhost:2019",
		"http://localhost:2019/":  "http://localhost:2019",
		" http://127.0.0.1:2019 ": "http://127.0.0.1:2019", //nolint:gocritic // surrounding whitespace is under test
		"https://caddy.lan:2019":  "https://caddy.lan:2019",
		"http://[::1]:2019":       "http://[::1]:2019",
	}
	for in, want := range valid {
		u, err := ParseURL(in)
		if err != nil {
			t.Errorf("ParseURL(%q) error = %v", in, err)
			continue
		}
		if u.String() != want {
			t.Errorf("ParseURL(%q) = %q, want %q", in, u.String(), want)
		}
	}

	invalid := []string{
		"",
		"localhost:2019",
		"unix:///run/caddy/admin.sock",
		"ftp://localhost:2019",
		"http://",
		"http://localhost:2019/config/",
		"http://localhost:2019?x=1",
		"http://user:pass@localhost:2019",
	}
	for _, in := range invalid {
		if _, err := ParseURL(in); err == nil {
			t.Errorf("ParseURL(%q) error = nil, want error", in)
		}
	}
}
