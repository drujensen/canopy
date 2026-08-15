package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

const (
	defaultFetchMaxBytes = 512 * 1024
	defaultFetchTimeout  = 30 * time.Second
	dialTimeout          = 10 * time.Second
)

// WebFetchConfig configures NewWebFetchTool.
type WebFetchConfig struct {
	// HTTPClient is the client used to issue requests. Defaults to a client
	// with defaultFetchTimeout and the SSRF guard (see newSafeTransport)
	// when nil.
	//
	// Security note: the SSRF guard below (rejecting loopback/link-
	// local/private/multicast destinations) is only wired into the default
	// client this package builds. A caller that supplies its own
	// HTTPClient is trusted to have made its own decision about what that
	// client is allowed to reach — Canopy's own production wiring
	// (domain/services.AgentService) never sets HTTPClient, so the real
	// deployed code path always gets the guarded default. This field exists
	// for tests (pointing at an httptest.Server, which is loopback and
	// would otherwise be rejected by the guard) and for a future deployment
	// hook, not as a way to opt out of the guard from the model side — the
	// model only ever supplies the URL, never Go code.
	HTTPClient *http.Client

	// MaxBytes caps how much of the response body is read. Zero uses
	// defaultFetchMaxBytes (512 KiB).
	MaxBytes int64

	// allowPrivateNetworks disables the SSRF guard's IP-range check for the
	// default client this package builds (it has no effect when HTTPClient
	// is set, since the guard doesn't apply there anyway). Unexported: only
	// this package's own tests (in web_fetch_test.go, same package) can set
	// it — there is no way for domain/services or any other caller to
	// reach this field, so it can never be used to weaken production
	// behavior. It exists so a test can exercise the guarded default
	// client's full request/response pipeline (dial through
	// newSafeTransport, read body, truncate) end-to-end against an
	// httptest.Server without that loopback address being rejected, while a
	// separate test (without this flag) proves the guard actually rejects
	// loopback/private/link-local destinations by default.
	allowPrivateNetworks bool
}

// WebFetchInput is the model-facing input for the web-fetch tool.
type WebFetchInput struct {
	URL string `json:"url" jsonschema:"The http(s) URL to fetch."`
}

// WebFetchOutput is the model-facing output for the web-fetch tool.
type WebFetchOutput struct {
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
	Truncated   bool   `json:"truncated"`
}

// NewWebFetchTool builds Canopy's web-fetch tool (docs/DESIGN.md §3.2 row
// 7): given a URL, returns its raw response body (capped at MaxBytes),
// status code, and content type, so the model can read a webpage. Not
// approval-gated — fetching is non-mutating (it's a GET, not a form submit).
//
// Deliberately scoped down: only http/https schemes are allowed (no file://,
// no reaching into the local network stack indirection like gopher:// etc.);
// there is no HTML-to-text/markdown conversion (unlike Claude Code's own
// WebFetch) — that's judged out of scope for a narrow v1 tool and can be
// layered on later without changing the interface.
//
// # SSRF guard (docs/PLAN.md Phase 7)
//
// The model chooses the URL this tool fetches, so without a guard it could
// be used to reach services Canopy's own process can see but the user never
// intended to expose to the model: cloud-provider instance-metadata
// endpoints (e.g. http://169.254.169.254/), localhost-bound admin panels,
// or anything else on a private/internal network. When cfg.HTTPClient is
// nil (the real production path — see WebFetchConfig.HTTPClient), the
// client this function builds uses newSafeTransport, whose DialContext
// rejects loopback/link-local/private/unspecified/multicast destinations —
// see isBlockedIP for the exact ranges and safeDialControl's doc comment
// for why the check happens at dial time (against the literal IP the OS is
// about to connect to) rather than as a one-time hostname lookup before the
// request: a pre-check alone is vulnerable to DNS rebinding (the name
// resolves to a public IP at check time, then to 169.254.169.254 by the
// time the request actually dials) and wouldn't cover a redirect to an
// internal address either, since the same Transport (and therefore the same
// dial guard) is reused for every connection a single request's redirects
// make.
func NewWebFetchTool(cfg WebFetchConfig) tool.FuncTool {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout:   defaultFetchTimeout,
			Transport: newSafeTransport(cfg.allowPrivateNetworks),
		}
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultFetchMaxBytes
	}

	return functool.MustNew(functool.Config{
		Name:        "web_fetch",
		Description: "Fetch the contents of a web page or other HTTP(S) resource at a given URL.",
	}, func(ctx context.Context, in WebFetchInput) (WebFetchOutput, error) {
		parsed, err := url.Parse(in.URL)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: invalid URL: %w", in.URL, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: unsupported URL scheme %q (only http/https allowed)", in.URL, parsed.Scheme)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: %w", in.URL, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: %w", in.URL, err)
		}
		defer resp.Body.Close()

		limited := io.LimitReader(resp.Body, maxBytes+1)
		body, err := io.ReadAll(limited)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: %w", in.URL, err)
		}

		truncated := int64(len(body)) > maxBytes
		if truncated {
			body = body[:maxBytes]
		}

		return WebFetchOutput{
			StatusCode:  resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Content:     string(body),
			Truncated:   truncated,
		}, nil
	})
}

// newSafeTransport builds an *http.Transport whose DialContext refuses to
// connect to a blocked destination (see isBlockedIP) for every connection it
// makes — the initial request and any redirect target, since http.Client
// reuses the same Transport (and therefore the same dial guard) across a
// request's whole redirect chain. allowPrivateNetworks disables the check
// entirely; only this package's own tests can request that (see
// WebFetchConfig.allowPrivateNetworks).
func newSafeTransport(allowPrivateNetworks bool) *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout}
	if !allowPrivateNetworks {
		dialer.Control = safeDialControl
	}
	// Clone the default transport rather than a bare &http.Transport{} so
	// unrelated behavior (proxy-from-environment, connection pooling
	// timeouts, TLS defaults, HTTP/2) matches what a plain http.Client{}
	// would have used — the only thing this function changes is DialContext.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	return transport
}

// safeDialControl is a net.Dialer.Control callback: the net package calls it
// once per dial attempt, after resolving the target to a literal IP address
// but before the connect() syscall, with that literal address — never a
// hostname. That timing is exactly why this check (rather than a one-time
// net.LookupIP before issuing the request) is safe against DNS rebinding: it
// sees and validates the actual address about to be connected to on every
// single attempt, including ones made mid-request for a redirect, instead of
// trusting a resolution that could legitimately differ by the time the
// connection is actually made.
func safeDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("web fetch: refusing to dial %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Should be unreachable: by the time Control is called the address
		// has already been resolved to a literal IP by the net package.
		// Fail closed rather than assume it's safe.
		return fmt.Errorf("web fetch: refusing to dial non-literal address %q", host)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("web fetch: refusing to dial internal/private/link-local address %s", ip)
	}
	return nil
}

// cgnatBlock is the IPv4 "shared address space" range (RFC 6598,
// 100.64.0.0/10) used for carrier-grade NAT — not covered by net.IP's own
// IsPrivate (which only implements RFC 1918 for IPv4), but reachable from a
// host the way RFC1918 space is, and worth denying for the same SSRF-guard
// reasons.
var cgnatBlock = func() *net.IPNet {
	_, block, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err)
	}
	return block
}()

// isBlockedIP reports whether ip is a destination the web-fetch tool's
// default client must never connect to: loopback (127.0.0.0/8, ::1),
// link-local unicast (169.254.0.0/16 — this is what covers cloud
// instance-metadata endpoints like 169.254.169.254 — and fe80::/10),
// link-local/interface-local multicast, private space (RFC 1918 IPv4;
// unique-local fc00::/7 IPv6), carrier-grade-NAT shared space (RFC 6598,
// 100.64.0.0/10), unspecified (0.0.0.0, ::), and multicast generally. A nil
// IP is treated as blocked (fail closed).
//
// Deliberately not attempting a truly exhaustive bogon list (e.g. every
// IANA-reserved block) — Requirements §7's actual threat here is a
// model-chosen URL reaching the host's own network-local services
// (metadata endpoints, localhost admin ports, internal LAN hosts), and the
// ranges above cover every address class that can mean "this box" or "this
// private network" to the OS's own routing, which is what those services
// sit behind.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		cgnatBlock.Contains(ip)
}
