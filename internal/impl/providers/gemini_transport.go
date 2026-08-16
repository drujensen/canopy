package providers

// This file works around a confirmed bug in the real Gemini API's wire
// format that agent-framework-go's provider/geminiprovider cannot tolerate:
// google.golang.org/genai's FunctionCall.ID is documented Optional
// (types.go: `ID string \`json:"id,omitempty"\``) and the real Gemini API
// routinely omits it entirely on both non-streaming (generateContent) and
// streaming (streamGenerateContent) responses. geminiprovider parses that
// missing field straight into *message.FunctionCallContent.CallID == "" (see
// agent.go:525, `CallID: part.FunctionCall.ID`) and its own request-building
// unconditionally errors on a *message.FunctionResultContent whose CallID is
// "" with "geminiprovider: missing function name for result with call ID
// \"\"".
//
// domain/services.withReconstructedApprovalFunctionCalls already works
// around this for the *cross-turn* case (a user answering a pending
// *message.ToolApprovalRequestContent* from a prior RunMessages/
// RunMessagesStream call), by patching the []*message.Message list Canopy's
// own application code hands to a.Run. That approach has no reach into
// agent/harness/toolautocall's *same-turn*, non-approval-gated auto-call
// loop: for every core tool that isn't Bash/FileWrite/MCP-provided (FileRead,
// FileSearch, DirectoryList, WebFetch, WebSearch, Skill), toolautocall
// invokes the tool and feeds the result back to the provider entirely inside
// one call to a.Run — Canopy's RunMessages/RunMessagesStream never sees the
// intermediate messages that loop constructs, so there is no []*message.Message
// hook to patch before the fact.
//
// The fix here operates one layer lower: a custom http.RoundTripper, wired
// into newGemini's *genai.ClientConfig via HTTPClient, inspects every
// generateContent/streamGenerateContent response's raw JSON bytes and injects
// a synthetic "id" into any "functionCall" object that's missing one, before
// genai's own decoder (api_client.go's deserializeUnaryResponse/
// deserializeStreamResponse) ever parses it. Because this runs underneath
// genai's decoding entirely, every *message.FunctionCallContent Canopy's own
// code (including toolautocall's same-turn loop, which this transport can't
// otherwise reach) ever sees already has a non-empty CallID by construction —
// see newGemini's doc comment for what this means for
// withReconstructedApprovalFunctionCalls' own empty-CallID branch.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// geminiFunctionCallIDPrefix names every synthetic ID this transport mints,
// distinct from withReconstructedApprovalFunctionCalls' own
// "canopy-synth-%d" prefix (domain/services/agent_service.go) so a synthetic
// ID's origin is unambiguous if it ever shows up in a log or a captured
// request body during debugging.
const geminiFunctionCallIDPrefix = "canopy-transport-fc-"

// newGeminiFunctionCallIDPatchingTransport wraps base (following
// impl/tools/web_fetch.go's newSafeTransport precedent: a nil base clones
// http.DefaultTransport rather than starting from a bare &http.Transport{},
// so unrelated behavior — proxy-from-environment, connection pooling,
// HTTP/2, TLS defaults — matches what a plain http.Client{} would already
// use) with a RoundTripper that repairs missing Gemini functionCall.id
// fields on the way back from the real API. See this file's package-level
// doc comment for the full motivation.
func newGeminiFunctionCallIDPatchingTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &geminiFunctionCallIDPatchingTransport{base: base}
}

// geminiFunctionCallIDPatchingTransport is the http.RoundTripper installed
// on the *genai.Client Canopy constructs for every Gemini provider (see
// newGemini). It only ever touches generateContent/streamGenerateContent
// response bodies (geminiRequestKind) on a 2xx response, and fails open —
// passing the response through completely unmodified — on any non-2xx
// status, any request that doesn't look like a generateContent call, any
// response body that doesn't contain a "functionCall" at all (the
// overwhelmingly common case: plain text turns, or turns with no tool calls),
// and any body that fails to parse the way real Gemini traffic is expected
// to. A bug in this patching logic must never turn a working response into a
// broken one, or swallow a real API error — see RoundTrip's own comments for
// where each of those guarantees is enforced.
type geminiFunctionCallIDPatchingTransport struct {
	base http.RoundTripper
}

func (t *geminiFunctionCallIDPatchingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		// Fail open: a transport-level error (or a nil response, which
		// shouldn't happen alongside a nil error but is defended against
		// anyway) is not this code's to interpret — propagate it unchanged.
		return resp, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A real API error (quota, auth, malformed request, ...) must reach
		// genai's own newAPIError unmodified — never risk corrupting an
		// error body by running it through JSON-shape-specific patching
		// logic that was never written with error responses in mind.
		return resp, nil
	}

	streaming, matched := geminiRequestKind(req.URL)
	if !matched {
		// Not a generateContent/streamGenerateContent request (e.g. listing
		// models, uploading a file, embeddings) — nothing here ever needs
		// patching, and every byte of the response is left completely
		// untouched, including resp.Body itself (never even wrapped).
		return resp, nil
	}

	if streaming {
		resp.Body = newGeminiSSEIDPatchingBody(resp.Body)
		return resp, nil
	}

	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		// The body couldn't even be fully read — this is a genuine
		// transport-level failure, not something patching logic should
		// paper over. Propagate it the same way a caller would have seen it
		// had this RoundTripper not been installed at all.
		return nil, fmt.Errorf("gemini function-call id transport: reading response body: %w", readErr)
	}

	patched, changed := patchGeminiFunctionCallIDsInJSONBody(body)
	if !changed {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(patched))
	resp.ContentLength = int64(len(patched))
	if resp.Header != nil {
		resp.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	}
	return resp, nil
}

// geminiRequestKind reports whether u names a Gemini
// generateContent-shaped endpoint and, if so, whether it's the streaming
// (streamGenerateContent) or non-streaming (generateContent) variant.
// models.go's Models.generateContent/generateContentStream build these paths
// as "{model}:generateContent" and "{model}:streamGenerateContent?alt=sse"
// respectively (confirmed by reading google.golang.org/genai@v1.68.0's
// models.go directly, not assumed) — the streaming suffix is checked first
// since ":streamGenerateContent" does NOT contain the exact substring
// ":generateContent" (the capital "G" immediately follows "stream", with no
// colon-lowercase-g in between), so the two are already mutually exclusive
// substrings and the order here is only for clarity, not correctness.
func geminiRequestKind(u *url.URL) (streaming, matched bool) {
	if u == nil {
		return false, false
	}
	switch {
	case strings.Contains(u.Path, ":streamGenerateContent"):
		return true, true
	case strings.Contains(u.Path, ":generateContent"):
		return false, true
	default:
		return false, false
	}
}

// patchGeminiFunctionCallIDsInJSONBody implements the non-streaming
// (generateContent) case: body is a single, complete JSON document
// (confirmed from api_client.go's deserializeUnaryResponse, which
// io.ReadAll's the entire resp.Body before ever calling json.Unmarshal — the
// SDK itself never streams a non-streaming response). Returns the original
// body unchanged (changed=false) whenever there's nothing to do or anything
// goes wrong parsing it — the fast path for the overwhelmingly common case
// (no function calls at all) is a cheap substring scan before ever paying
// for a full json.Unmarshal.
func patchGeminiFunctionCallIDsInJSONBody(body []byte) (out []byte, changed bool) {
	if !bytes.Contains(body, []byte("functionCall")) {
		return body, false
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Fail open: an unexpected/malformed body is not this function's to
		// diagnose — hand the original bytes back untouched so genai's own
		// decoder produces whatever error it would have produced anyway.
		return body, false
	}
	var counter int
	if !patchGeminiFunctionCallIDs(parsed, &counter) {
		return body, false
	}
	reMarshaled, err := json.Marshal(parsed)
	if err != nil {
		return body, false
	}
	return reMarshaled, true
}

// patchGeminiFunctionCallIDs recursively walks v (the generic
// map[string]any/[]any/... shape json.Unmarshal produces for `any`) looking
// for any object keyed "functionCall" whose own object value has no "id" key
// or an empty-string "id", and mints a synthetic, response-locally-unique ID
// for it via counter. Returns whether anything was changed, so callers can
// skip a needless re-marshal (which would lose the original response's key
// ordering — harmless to genai's own decoder, which doesn't care about map
// key order, but unnecessary work and an unnecessary diff from the original
// bytes) when nothing actually needed patching.
//
// This walks every object's every value unconditionally (not just
// "candidates"/"content"/"parts", the known nesting for a
// GenerateContentResponse) because that nesting is genai's *typed* shape,
// not a contract this raw-JSON transform should depend on — walking
// everything is more robust to a future response shape change (extra
// wrapper fields, a differently-nested batch/replay format, etc.) at
// negligible cost, since a real response body is never large enough for the
// extra recursion to matter.
func patchGeminiFunctionCallIDs(v any, counter *int) bool {
	changed := false
	switch val := v.(type) {
	case map[string]any:
		if fcRaw, ok := val["functionCall"]; ok {
			if fc, ok := fcRaw.(map[string]any); ok {
				if id, ok := fc["id"].(string); !ok || id == "" {
					*counter++
					fc["id"] = fmt.Sprintf("%s%d", geminiFunctionCallIDPrefix, *counter)
					changed = true
				}
			}
		}
		for _, sub := range val {
			if patchGeminiFunctionCallIDs(sub, counter) {
				changed = true
			}
		}
	case []any:
		for _, item := range val {
			if patchGeminiFunctionCallIDs(item, counter) {
				changed = true
			}
		}
	}
	return changed
}

// newGeminiSSEIDPatchingBody wraps orig — the raw, still-unparsed response
// body of a streamGenerateContent request — with an io.ReadCloser that
// re-emits the exact same Server-Sent-Events framing, patching only the
// "functionCall" objects that need it.
//
// # Streaming-safety: can a functionCall ever split across two chunks?
//
// No, confirmed directly from google.golang.org/genai@v1.68.0's own
// deserializeStreamResponse/iterateResponseStream (api_client.go): the SDK
// reads the stream with a bufio.Scanner whose split function (scan, in the
// same file) advances one full event at a time, delimited by a blank line
// ("\n\n" or, defensively, "\r\n\r\n") — this is standard Server-Sent-Events
// framing, not token-level text streaming. iterateResponseStream then does
// `json.Unmarshal(data, &respRaw)` on each event's data in one shot, with no
// buffering/concatenation across events at all — if an event's data weren't
// a complete, individually-valid JSON document, the SDK's own unmarshal
// would fail for genuine Gemini traffic on every single streamed function
// call, which it demonstrably doesn't (function calls, unlike token-streamed
// text, aren't the kind of content Gemini's own streaming protocol splits
// mid-object). So a single blank-line-delimited SSE event is guaranteed to
// carry a complete top-level JSON document, and therefore any "functionCall"
// object nested inside it is guaranteed complete too — a per-event
// buffer-and-patch approach (mirroring the SDK's own event boundary exactly,
// see splitGeminiSSERaw below) is safe.
//
// # Why this stays truly streaming rather than buffering the whole body
//
// Buffering the entire stream up front before patching would be simpler,
// but would defeat streaming's actual purpose (incremental text arriving as
// the model produces it, which Canopy's TUI renders live) — the caller would
// see nothing until the whole response had already finished. Instead this
// reads and re-emits one SSE event at a time via an io.Pipe: a goroutine
// scans orig for the next event, patches it if (and only if) it actually
// contains "functionCall" (same cheap fast path as the non-streaming case,
// applied per-event here since the whole body is never buffered at once to
// check), and writes it to the pipe — which blocks until the caller's own
// Read has consumed it, so at most one event is ever held in memory ahead of
// the reader. Every event that doesn't need patching is written back with
// its own original, unmodified bytes (including its original delimiter) —
// only an event that actually contains and needs a patched functionCall.id
// is rebuilt.
func newGeminiSSEIDPatchingBody(orig io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = orig.Close() }()
		scanner := bufio.NewScanner(orig)
		// Mirror api_client.go's deserializeStreamResponse buffer sizing
		// (1KB initial, grown up to 256MB) so this never rejects a valid
		// event the real SDK's own scanner would have accepted.
		scanner.Buffer(make([]byte, 1024), 256*1024*1024)
		scanner.Split(splitGeminiSSERaw)

		var counter int
		for scanner.Scan() {
			// scanner.Bytes() is reused on the next Scan() call, but it's
			// only read here (never retained past this iteration) and the
			// pw.Write below blocks until the reader has fully consumed it
			// — by the time Write returns, this iteration is done with the
			// slice, so no copy is needed before the next Scan() reuses it.
			out := patchGeminiSSEToken(scanner.Bytes(), &counter)
			if _, err := pw.Write(out); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return pr
}

// splitGeminiSSERaw is a bufio.SplitFunc mirroring genai's own unexported
// `scan` function (api_client.go) exactly in how it finds event boundaries
// ("\n\n", then defensively "\r\n\r\n", then whatever's left at EOF), but —
// unlike genai's version, which strips the delimiter and any trailing "\r"
// since it only needs the event's content — this returns the token WITH its
// original trailing delimiter still attached (and without CR-stripping), so
// an event that doesn't need patching can be re-emitted with its exact
// original bytes, delimiter included, rather than a reconstructed one.
func splitGeminiSSERaw(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[0 : i+2], nil
	}
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return i + 4, data[0 : i+4], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// patchGeminiSSEToken processes one splitGeminiSSERaw token (an SSE event,
// original trailing delimiter still attached, or a final undelimited
// fragment at EOF). It returns raw byte-for-byte unchanged whenever there's
// nothing to patch or anything about the token doesn't match the expected
// "data:<json>" shape (fail open, same posture as
// patchGeminiFunctionCallIDsInJSONBody) — only an event that both parses as
// JSON and actually contains a functionCall needing an id is rebuilt, and
// even then only its JSON payload changes; the event's own "data" prefix and
// delimiter bytes are preserved exactly as received.
func patchGeminiSSEToken(raw []byte, counter *int) []byte {
	var delim []byte
	content := raw
	switch {
	case bytes.HasSuffix(raw, []byte("\r\n\r\n")):
		delim = raw[len(raw)-4:]
		content = raw[:len(raw)-4]
	case bytes.HasSuffix(raw, []byte("\n\n")):
		delim = raw[len(raw)-2:]
		content = raw[:len(raw)-2]
	}

	if len(bytes.TrimSpace(content)) == 0 || !bytes.Contains(content, []byte("functionCall")) {
		return raw
	}

	prefix, data, found := bytes.Cut(content, []byte(":"))
	if !found || string(bytes.TrimSpace(prefix)) != "data" {
		// Not a recognized "data:" event (e.g. an SSE comment line starting
		// with ':', or an "event:"/other field this transform doesn't need
		// to understand) — pass it through exactly as received.
		return raw
	}

	var parsed any
	if err := json.Unmarshal(bytes.TrimSpace(data), &parsed); err != nil {
		return raw
	}
	if !patchGeminiFunctionCallIDs(parsed, counter) {
		return raw
	}
	reMarshaled, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}

	out := make([]byte, 0, len(prefix)+1+len(reMarshaled)+len(delim))
	out = append(out, prefix...)
	out = append(out, ':')
	out = append(out, reMarshaled...)
	out = append(out, delim...)
	return out
}
