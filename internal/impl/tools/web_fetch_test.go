package tools

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWebFetchTool_Contract(t *testing.T) {
	ft := NewWebFetchTool(WebFetchConfig{})

	var _ tool.FuncTool = ft
	assert.Equal(t, "web_fetch", ft.Name())
	assert.NotEmpty(t, ft.Description())

	_, approvalGated := ft.(tool.ApprovalRequiredTool)
	assert.False(t, approvalGated, "web fetch must not be approval-gated")
}

func TestNewWebFetchTool_FetchesRealLocalServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from the web"))
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{HTTPClient: srv.Client()})

	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(WebFetchOutput)
	require.True(t, ok, "expected WebFetchOutput, got %T", out)
	assert.Equal(t, http.StatusOK, result.StatusCode)
	assert.Equal(t, "hello from the web", result.Content)
	assert.Contains(t, result.ContentType, "text/plain")
	assert.False(t, result.Truncated)
}

func TestNewWebFetchTool_TruncatesLargeBody(t *testing.T) {
	body := strings.Repeat("x", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{HTTPClient: srv.Client(), MaxBytes: 10})

	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result := out.(WebFetchOutput)
	assert.Len(t, result.Content, 10)
	assert.True(t, result.Truncated)
}

func TestNewWebFetchTool_RejectsNonHTTPScheme(t *testing.T) {
	ft := NewWebFetchTool(WebFetchConfig{})

	args, err := json.Marshal(map[string]string{"url": "file:///etc/passwd"})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported URL scheme")
}

func TestNewWebFetchTool_PropagatesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{HTTPClient: srv.Client()})
	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err, "a non-2xx response is not a Go error, just reported via StatusCode")

	result := out.(WebFetchOutput)
	assert.Equal(t, http.StatusNotFound, result.StatusCode)
}

// TestIsBlockedIP is the primary, network-free proof of the SSRF guard's
// core allow/deny logic (docs/PLAN.md Phase 7): every address class that
// must never be reachable through a model-chosen web_fetch URL — loopback,
// link-local (including the cloud instance-metadata address
// 169.254.169.254), RFC1918/unique-local private space, CGNAT shared space,
// unspecified, and multicast — is rejected, while ordinary public addresses
// are not.
func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"IPv4 loopback", "127.0.0.1", true},
		{"IPv4 loopback range", "127.255.255.254", true},
		{"IPv6 loopback", "::1", true},
		{"IPv4-mapped IPv6 loopback", "::ffff:127.0.0.1", true},
		{"link-local unicast (cloud metadata)", "169.254.169.254", true},
		{"link-local unicast base", "169.254.0.1", true},
		{"IPv6 link-local unicast", "fe80::1", true},
		{"private 10/8", "10.0.0.1", true},
		{"private 172.16/12", "172.16.5.4", true},
		{"private 192.168/16", "192.168.1.1", true},
		{"IPv6 unique local", "fd00::1", true},
		{"CGNAT shared space", "100.64.0.1", true},
		{"CGNAT shared space upper bound", "100.127.255.254", true},
		{"unspecified IPv4", "0.0.0.0", true},
		{"unspecified IPv6", "::", true},
		{"multicast", "224.0.0.1", true},
		{"public IPv4 (Google DNS)", "8.8.8.8", false},
		{"public IPv4 (Cloudflare DNS)", "1.1.1.1", false},
		{"public IPv4 just outside 172.16/12", "172.32.0.1", false},
		{"public IPv4 just outside CGNAT", "100.128.0.1", false},
		{"public IPv6", "2001:4860:4860::8888", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			require.NotNil(t, ip, "test IP %q must parse", tc.ip)
			assert.Equal(t, tc.blocked, isBlockedIP(ip), "isBlockedIP(%s)", tc.ip)
		})
	}
}

func TestIsBlockedIP_NilTreatedAsBlocked(t *testing.T) {
	assert.True(t, isBlockedIP(nil), "a nil/unresolved IP must fail closed")
}

// TestNewWebFetchTool_DefaultClient_RejectsLoopback proves the SSRF guard is
// actually wired into the tool's real (production) code path: a
// WebFetchConfig with no HTTPClient — the only way domain/services.
// AgentService ever constructs this tool — must reject a URL that resolves
// to loopback before ever reaching the target server. The target is a real
// httptest.Server (bound to 127.0.0.1) specifically so the handler running
// can prove it was never invoked, not just that some error came back.
func TestNewWebFetchTool_DefaultClient_RejectsLoopback(t *testing.T) {
	var handlerCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{})

	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err, "fetching a loopback-bound server through the default client must be rejected")
	assert.False(t, handlerCalled, "the request must never have reached the server")
}

// TestNewWebFetchTool_DefaultClient_RejectsLiteralInternalAddresses hits the
// guard directly with literal IP-address URLs for the address classes
// Requirements §7/PLAN.md Phase 7 call out by name (cloud metadata,
// localhost-equivalent, private ranges). These need no listener at all —
// net.Dialer.Control fires before the connect() syscall — so the test is
// fast and deterministic even with no network access in CI.
func TestNewWebFetchTool_DefaultClient_RejectsLiteralInternalAddresses(t *testing.T) {
	ft := NewWebFetchTool(WebFetchConfig{})

	urls := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:80/",
		"http://[::1]/",
		"http://10.0.0.5/",
		"http://172.16.0.5/",
		"http://192.168.0.5/",
		"http://0.0.0.0/",
	}
	for _, u := range urls {
		t.Run(u, func(t *testing.T) {
			args, err := json.Marshal(map[string]string{"url": u})
			require.NoError(t, err)

			_, err = ft.Call(context.Background(), string(args))
			require.Error(t, err, "fetching %q through the default client must be rejected", u)
			assert.Contains(t, err.Error(), "refusing to dial")
		})
	}
}

// TestNewWebFetchTool_DefaultClient_AllowsLegitimateAddress_EndToEnd proves
// the guard doesn't just block — it lets a request to a non-blocked address
// complete normally through the exact same guarded code path (newSafeTransport's
// DialContext, then read/truncate the body). It uses the package-private
// allowPrivateNetworks test hook (only reachable from this file, since it's
// an unexported WebFetchConfig field — domain/services and every other
// caller outside package tools cannot set it) to treat httptest.Server's
// loopback address as allowed, standing in for "some legitimate external
// address" without requiring live internet access from CI. This is deliberately
// exercising the *safety-wrapped* transport (not a bypass via a
// caller-supplied HTTPClient, which the other tests in this file use for
// unrelated HTTP-handling behavior) — see WebFetchConfig.allowPrivateNetworks's
// doc comment for why this is a safe, package-scoped test seam rather than a
// production escape hatch.
func TestNewWebFetchTool_DefaultClient_AllowsLegitimateAddress_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from a legitimate address"))
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{allowPrivateNetworks: true})

	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err, "a non-blocked address must still be fetched normally through the guarded client")

	result, ok := out.(WebFetchOutput)
	require.True(t, ok)
	assert.Equal(t, http.StatusOK, result.StatusCode)
	assert.Equal(t, "hello from a legitimate address", result.Content)
}
