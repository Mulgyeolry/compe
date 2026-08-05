package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"competition-assistant/internal/config"
)

// TestIPClassification covers the private/reserved addresses that must be
// rejected and the public addresses that must be allowed.
func TestIPClassification(t *testing.T) {
	rejected := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0",
		"::1", "fc00::1", "fe80::1", "ff02::1",
	}
	for _, raw := range rejected {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if isSafeIP(addr) {
			t.Errorf("isSafeIP(%q) = true, want false", raw)
		}
	}
	allowed := []string{
		"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111",
	}
	for _, raw := range allowed {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if !isSafeIP(addr) {
			t.Errorf("isSafeIP(%q) = false, want true", raw)
		}
	}
	// IPv4-mapped IPv6 loopback must be recognized as unsafe.
	mapped := netip.MustParseAddr("::ffff:127.0.0.1")
	if isSafeIP(mapped) {
		t.Error("IPv4-mapped IPv6 loopback must be rejected")
	}
}

// TestProductionPublicClientRejectsLoopback verifies the production collector
// built with NewHTTPCollector refuses a loopback httptest server.
func TestProductionPublicClientRejectsLoopback(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	collector := NewHTTPCollector(config.Config{Fetch: config.Fetch{TimeoutSeconds: 5, MaxBytes: 1 << 20}})
	_, err := collector.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("Fetch(loopback) err = %v, want ErrUnsafeURL", err)
	}
	if hit {
		t.Fatal("the loopback server handler must not have been reached")
	}
}

// TestResolveToPrivateIsRejected injects a fake resolver that maps a public
// hostname to a private address and asserts the request is rejected without
// dialing.
func TestResolveToPrivateIsRejected(t *testing.T) {
	dialed := false
	transport := publicRoundTripper(
		func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("should not dial")
		},
	)
	client := &http.Client{Transport: transport}
	req, _ := http.NewRequest(http.MethodGet, "http://contest.example.com/x", nil)
	_, err := client.Do(req)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("client.Do err = %v, want ErrUnsafeURL", err)
	}
	if dialed {
		t.Fatal("dial must not happen when resolution yields a private address")
	}
}

// TestDialUsesValidatedIP proves the transport connects to the resolved public
// IP rather than re-resolving the hostname (preventing DNS rebinding).
func TestDialUsesValidatedIP(t *testing.T) {
	var dialedAddr string
	transport := publicRoundTripper(
		func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		},
		func(_ context.Context, _, address string) (net.Conn, error) {
			dialedAddr = address
			return nil, errors.New("stop here")
		},
	)
	client := &http.Client{Transport: transport}
	req, _ := http.NewRequest(http.MethodGet, "http://contest.example.com/", nil)
	_, _ = client.Do(req)
	if !strings.HasPrefix(dialedAddr, "1.1.1.1:") {
		t.Fatalf("dialed address = %q, want 1.1.1.1:80 (no re-resolution)", dialedAddr)
	}
}

// TestRedirectToPrivateIsRejected verifies the public redirect policy refuses a
// redirect to a private/metadata address.
func TestRedirectToPrivateIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, nil, "http://127.0.0.1/admin", http.StatusFound)
	}))
	defer server.Close()
	collector := NewHTTPCollector(config.Config{Fetch: config.Fetch{TimeoutSeconds: 5, MaxBytes: 1 << 20}})
	_, err := collector.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("redirect to loopback err = %v, want ErrUnsafeURL", err)
	}
}

// TestRedirectLoopIsRejected verifies the public redirect policy stops a
// redirect chain that exceeds the maximum hop count.
func TestRedirectLoopIsRejected(t *testing.T) {
	// /a -> /b -> /c -> /a ... endless loop; the limit must stop it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := map[string]string{"/a": "/b", "/b": "/c", "/c": "/a"}[r.URL.Path]
		http.Redirect(w, nil, next, http.StatusFound)
	}))
	defer server.Close()
	collector := NewHTTPCollector(config.Config{Fetch: config.Fetch{TimeoutSeconds: 5, MaxBytes: 1 << 20}})
	_, err := collector.Fetch(context.Background(), server.URL+"/a")
	if err == nil {
		t.Fatal("redirect loop must be rejected")
	}
}

// TestUnsafeErrorIsNotRetried verifies a security rejection returns immediately
// without exponential-backoff retries.
func TestUnsafeErrorIsNotRetried(t *testing.T) {
	attempts := 0
	transport := &countRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			attempts++
			return nil, fmt.Errorf("%w: blocked", ErrUnsafeURL)
		},
	}
	collector := &HTTPCollector{
		client:     &http.Client{Transport: transport},
		maxRetries: 3,
	}
	_, err := collector.doRequest(context.Background(), "http://contest.example.com/x", nil)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("doRequest err = %v, want ErrUnsafeURL", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (unsafe errors must not be retried)", attempts)
	}
}

// countRoundTripper counts how many times RoundTrip is invoked.
type countRoundTripper struct {
	fn func(*http.Request) (*http.Response, error)
}

func (c *countRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return c.fn(req)
}

// TestSearxngInternalAddressRemainsReachable verifies the trusted service client
// can still talk to an in-network SearxNG instance.
func TestSearxngInternalAddressRemainsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"2026 软件开发大赛","url":"https://contest.example.com/2026","content":"报名通知"}]}`))
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	collector := NewHTTPCollectorForTest(parsed)
	candidates, err := collector.Discover(context.Background(), config.Source{ID: "s", Name: "s", Kind: "search", Query: "软件开发 大赛", Limit: 5})
	if err != nil {
		t.Fatalf("Discover(search) err = %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
}

// TestPublicURLRejections covers malformed or unsafe URL forms.
func TestPublicURLRejections(t *testing.T) {
	bad := []string{
		"file:///etc/passwd",
		"ftp://example.com/file",
		"javascript:alert(1)",
		"http://localhost/admin",
		"http://user:pass@example.com/",
	}
	for _, raw := range bad {
		if _, err := validatePublicURL(raw); err == nil {
			t.Errorf("validatePublicURL(%q) accepted an unsafe URL", raw)
		}
	}
	good := "https://8.8.8.8/path"
	if _, err := validatePublicURL(good); err != nil {
		t.Errorf("validatePublicURL(%q) = %v, want nil", good, err)
	}
}
