package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
// redirect to a private/metadata address by testing publicCheckRedirect directly
// (a real public fetch would reject the initial loopback target before any
// redirect is followed).
func TestRedirectToPrivateIsRejected(t *testing.T) {
	previous := mustRequest(t, "https://public.example.com/start")
	cases := []struct {
		name string
		url  string
	}{
		{"loopback", "http://127.0.0.1/admin"},
		{"userinfo", "http://user:pass@public.example.com/x"},
		{"ftp scheme", "ftp://public.example.com/file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t, tc.url)
			err := publicCheckRedirect(req, []*http.Request{previous})
			if !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("publicCheckRedirect(%q) err = %v, want ErrUnsafeURL", tc.url, err)
			}
		})
	}
}

// TestRedirectLimitIsRejected verifies the public redirect policy stops a chain
// that exceeds the maximum hop count.
func TestRedirectLimitIsRejected(t *testing.T) {
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = mustRequest(t, "https://public.example.com/hop")
	}
	req := mustRequest(t, "https://public.example.com/next")
	err := publicCheckRedirect(req, via)
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("publicCheckRedirect err = %v, want 'too many redirects'", err)
	}
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
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

// TestSearxngInternalAddressRemainsReachable verifies the trusted service
// client of the PRODUCTION constructor can still talk to an in-network SearxNG
// instance on a private/loopback address.
func TestSearxngInternalAddressRemainsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"2026 软件开发大赛","url":"https://contest.example.com/2026","content":"报名通知"}]}`))
	}))
	defer server.Close()
	cfg := config.Config{
		SearxngURL: server.URL,
		Fetch:      config.Fetch{TimeoutSeconds: 5, MaxBytes: 1 << 20},
	}
	collector := NewHTTPCollector(cfg)
	candidates, err := collector.Discover(context.Background(), config.Source{ID: "s", Name: "s", Kind: "search", Query: "软件开发 大赛", Limit: 5})
	if err != nil {
		t.Fatalf("Discover(search) err = %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
}

// TestPublicRequestRejectsUnsafeTargetsAtEntry verifies the public entry points
// reject unsafe targets before any network I/O: validatePublicURL runs before
// the retry loop and dialing.
func TestPublicRequestRejectsUnsafeTargetsAtEntry(t *testing.T) {
	attempts := 0
	collector := &HTTPCollector{
		client: &http.Client{Transport: &countRoundTripper{fn: func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("should not be reached")
		}}},
		maxRetries: 2,
	}
	cases := []string{
		"file:///etc/passwd",
		"ftp://example.com/file",
		"http://user:pass@example.com/",
		"http://foo.localhost/",
	}
	for _, target := range cases {
		_, err := collector.doRequest(context.Background(), target, nil)
		if !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("doRequest(%q) err = %v, want ErrUnsafeURL", target, err)
		}
	}
	if attempts != 0 {
		t.Fatalf("RoundTripper invoked %d times, want 0 (rejected before network I/O)", attempts)
	}
}

// TestEmptyDNSResultIsHandledWithoutPanic verifies the safe transport returns a
// clear error and never dials when DNS returns no addresses.
func TestEmptyDNSResultIsHandledWithoutPanic(t *testing.T) {
	dialed := 0
	transport := publicRoundTripper(
		func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{}, nil
		},
		func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed++
			return nil, errors.New("must not dial")
		},
	)
	client := &http.Client{Transport: transport}
	req := mustRequest(t, "http://contest.example.com/x")
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("empty DNS result must yield an error, not a nil response")
	}
	if dialed != 0 {
		t.Fatalf("dialed %d times with empty DNS, want 0", dialed)
	}
}

// TestMultiplePublicIPsFallback verifies the transport tries every validated
// public IP in turn: the first dial fails and the second is attempted, so both
// validated IPs are seen by the dialer (never the hostname).
func TestMultiplePublicIPsFallback(t *testing.T) {
	var dialed []string
	transport := publicRoundTripper(
		func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("8.8.8.8")}}, nil
		},
		func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("all dials fail for fallback assertion")
		},
	)
	client := &http.Client{Transport: transport}
	req := mustRequest(t, "http://contest.example.com/x")
	_, _ = client.Do(req)
	if len(dialed) != 2 {
		t.Fatalf("dialed %d addresses, want 2 (both validated IPs tried)", len(dialed))
	}
	if !strings.HasPrefix(dialed[0], "1.1.1.1:") || !strings.HasPrefix(dialed[1], "8.8.8.8:") {
		t.Fatalf("dialed addresses = %v, want validated IPs not hostnames", dialed)
	}
}

// TestMixedPublicAndPrivateDNSIsRejected verifies a domain that resolves to a
// mix of public and private addresses is rejected entirely, with no dial.
func TestMixedPublicAndPrivateDNSIsRejected(t *testing.T) {
	dialed := 0
	transport := publicRoundTripper(
		func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("127.0.0.1")}}, nil
		},
		func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed++
			return nil, errors.New("must not dial")
		},
	)
	client := &http.Client{Transport: transport}
	req := mustRequest(t, "http://contest.example.com/x")
	_, err := client.Do(req)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("client.Do err = %v, want ErrUnsafeURL", err)
	}
	if dialed != 0 {
		t.Fatalf("dialed %d times with a mixed public/private result, want 0", dialed)
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
