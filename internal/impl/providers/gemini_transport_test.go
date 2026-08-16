package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// geminiRequestKind
// ---------------------------------------------------------------------

func TestGeminiRequestKind(t *testing.T) {
	cases := []struct {
		name          string
		rawURL        string
		wantStreaming bool
		wantMatched   bool
	}{
		{
			name:          "non-streaming generateContent",
			rawURL:        "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
			wantStreaming: false,
			wantMatched:   true,
		},
		{
			name:          "streaming streamGenerateContent with alt=sse",
			rawURL:        "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
			wantStreaming: true,
			wantMatched:   true,
		},
		{
			name:          "streaming streamGenerateContent without query string",
			rawURL:        "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent",
			wantStreaming: true,
			wantMatched:   true,
		},
		{
			name:          "unrelated endpoint: list models",
			rawURL:        "https://generativelanguage.googleapis.com/v1beta/models",
			wantStreaming: false,
			wantMatched:   false,
		},
		{
			name:          "unrelated endpoint: embedContent",
			rawURL:        "https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:embedContent",
			wantStreaming: false,
			wantMatched:   false,
		},
		{
			name:          "unrelated endpoint: file upload",
			rawURL:        "https://generativelanguage.googleapis.com/upload/v1beta/files",
			wantStreaming: false,
			wantMatched:   false,
		},
		{
			name:          "countTokens is not generateContent",
			rawURL:        "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:countTokens",
			wantStreaming: false,
			wantMatched:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.rawURL)
			require.NoError(t, err)
			streaming, matched := geminiRequestKind(u)
			assert.Equal(t, tc.wantMatched, matched, "matched")
			if matched {
				assert.Equal(t, tc.wantStreaming, streaming, "streaming")
			}
		})
	}

	t.Run("nil URL", func(t *testing.T) {
		streaming, matched := geminiRequestKind(nil)
		assert.False(t, streaming)
		assert.False(t, matched)
	})
}

// ---------------------------------------------------------------------
// patchGeminiFunctionCallIDsInJSONBody / patchGeminiFunctionCallIDs
// ---------------------------------------------------------------------

// realisticGeminiFunctionCallBody is modeled directly on the raw bytes
// captured from a real streamGenerateContent call during this task's live
// verification probe (a single event containing two functionCall parts, one
// with a large base64 thoughtSignature) — see gemini_transport.go's package
// doc comment for the underlying bug. Both functionCall objects have no
// "id" field at all, matching real Gemini traffic.
const realisticGeminiFunctionCallBody = `{"candidates": [{"content": {"parts": [{"functionCall": {"name": "list_directory","args": {"path": "/tmp"}},"thoughtSignature": "CiQBEU0yD7pXvs4wv+3ZLLv0dNW12KR"},{"functionCall": {"name": "read_file","args": {"path": "/tmp/foo.txt"}}}],"role": "model"},"finishReason": "STOP","index": 0,"finishMessage": "Model generated function call(s)."}],"usageMetadata": {"promptTokenCount": 103,"candidatesTokenCount": 34,"totalTokenCount": 188},"modelVersion": "gemini-2.5-flash","responseId": "_V2BapveKcPg8QGa35aRCw"}`

func TestPatchGeminiFunctionCallIDsInJSONBody_MissingID(t *testing.T) {
	out, changed := patchGeminiFunctionCallIDsInJSONBody([]byte(realisticGeminiFunctionCallBody))
	require.True(t, changed)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))

	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 2, "both functionCall objects must have been visited")
	for _, id := range ids {
		assert.NotEmpty(t, id)
		assert.True(t, strings.HasPrefix(id, geminiFunctionCallIDPrefix), "id %q must carry the transport's synthetic-id prefix", id)
	}
	// Distinct, non-colliding IDs for multiple function calls in one response.
	assert.NotEqual(t, ids[0], ids[1])

	// The rest of the body must be untouched: same text/other fields.
	assertJSONEqualExceptFunctionCallID(t, realisticGeminiFunctionCallBody, string(out))
}

func TestPatchGeminiFunctionCallIDsInJSONBody_AlreadyPresent(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","id":"existing-id-123","args":{}}}],"role":"model"}}]}`
	out, changed := patchGeminiFunctionCallIDsInJSONBody([]byte(body))
	assert.False(t, changed)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 1)
	assert.Equal(t, "existing-id-123", ids[0], "an already-present id must never be overwritten")
}

func TestPatchGeminiFunctionCallIDsInJSONBody_EmptyStringID(t *testing.T) {
	// json omitempty on the wire means the field is normally absent, but the
	// code explicitly also treats an empty-string "id" as needing a patch
	// (fc["id"].(string); !ok || id == "") — cover that branch directly.
	body := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","id":"","args":{}}}],"role":"model"}}]}`
	out, changed := patchGeminiFunctionCallIDsInJSONBody([]byte(body))
	require.True(t, changed)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 1)
	assert.NotEmpty(t, ids[0])
}

func TestPatchGeminiFunctionCallIDsInJSONBody_NoFunctionCall_FastPath(t *testing.T) {
	// A text-only response must never even attempt to json.Unmarshal — prove
	// it by handing a body that would fail (or, worse, panic) if actually
	// parsed as JSON. If patchGeminiFunctionCallIDsInJSONBody's substring
	// fast-path is skipped, json.Unmarshal on this body returns a syntax
	// error, which the function already treats as "give up, unchanged" —
	// so to genuinely prove the *fast path* (not just "parsing failed
	// harmlessly"), we additionally check that the returned bytes are the
	// exact same underlying slice header (Go slices from the same backing
	// array/len/cap would still assert Equal on content; instead we rely on
	// the byte-for-byte content check plus changed=false, which is the
	// externally observable contract) and that no panic occurs.
	malformedNonFunctionCallBody := []byte(`{this is not valid json at all, no functionCall here`)
	out, changed := patchGeminiFunctionCallIDsInJSONBody(malformedNonFunctionCallBody)
	assert.False(t, changed)
	assert.Equal(t, malformedNonFunctionCallBody, out)
}

func TestPatchGeminiFunctionCallIDsInJSONBody_NoFunctionCall_TextOnly(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"hello there"}],"role":"model"},"finishReason":"STOP"}]}`
	out, changed := patchGeminiFunctionCallIDsInJSONBody([]byte(body))
	assert.False(t, changed)
	assert.Equal(t, body, string(out))
}

func TestPatchGeminiFunctionCallIDsInJSONBody_MalformedJSON_WithFunctionCallSubstring(t *testing.T) {
	// Contains the substring "functionCall" (so the fast path does NOT
	// short-circuit) but is not valid JSON — must fail open, unchanged.
	body := []byte(`{"functionCall": this is not valid json`)
	out, changed := patchGeminiFunctionCallIDsInJSONBody(body)
	assert.False(t, changed)
	assert.Equal(t, body, out)
}

// collectFunctionCallIDs walks a parsed JSON document (as produced by
// json.Unmarshal into `any`) and returns every "id" value found nested
// under a "functionCall" key, in traversal order. Used to assert on
// multiple, potentially-nested function calls without hard-coding the
// response shape.
func collectFunctionCallIDs(t *testing.T, v any) []string {
	t.Helper()
	var ids []string
	var walk func(any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			if fcRaw, ok := val["functionCall"]; ok {
				if fc, ok := fcRaw.(map[string]any); ok {
					id, _ := fc["id"].(string)
					ids = append(ids, id)
				}
			}
			for _, sub := range val {
				walk(sub)
			}
		case []any:
			for _, item := range val {
				walk(item)
			}
		}
	}
	walk(v)
	return ids
}

// assertJSONEqualExceptFunctionCallID parses both bodies and asserts they
// are structurally identical except that each functionCall object's "id"
// field may differ (was absent/empty in want, and non-empty in got).
func assertJSONEqualExceptFunctionCallID(t *testing.T, want, got string) {
	t.Helper()
	var wantParsed, gotParsed any
	require.NoError(t, json.Unmarshal([]byte(want), &wantParsed))
	require.NoError(t, json.Unmarshal([]byte(got), &gotParsed))
	normalizeFunctionCallIDs(wantParsed)
	normalizeFunctionCallIDs(gotParsed)
	assert.Equal(t, wantParsed, gotParsed, "response must be structurally identical apart from the injected functionCall id")
}

func normalizeFunctionCallIDs(v any) {
	switch val := v.(type) {
	case map[string]any:
		if fcRaw, ok := val["functionCall"]; ok {
			if fc, ok := fcRaw.(map[string]any); ok {
				// Delete rather than overwrite: the "want" side (original,
				// unpatched body) has no "id" key at all, while "got" (the
				// patched body) has a synthetic one — deleting on both sides
				// makes them comparable while still leaving every other
				// field (name, args, thoughtSignature, ...) in place to
				// catch any unintended change.
				delete(fc, "id")
			}
		}
		for _, sub := range val {
			normalizeFunctionCallIDs(sub)
		}
	case []any:
		for _, item := range val {
			normalizeFunctionCallIDs(item)
		}
	}
}

// ---------------------------------------------------------------------
// splitGeminiSSERaw
// ---------------------------------------------------------------------

func TestSplitGeminiSSERaw(t *testing.T) {
	t.Run("LF-delimited event, delimiter retained in token", func(t *testing.T) {
		data := []byte("data: {\"a\":1}\n\nmore")
		advance, token, err := splitGeminiSSERaw(data, false)
		require.NoError(t, err)
		assert.Equal(t, "data: {\"a\":1}\n\n", string(token))
		assert.Equal(t, len(token), advance)
	})

	t.Run("CRLF-delimited event, delimiter retained in token", func(t *testing.T) {
		data := []byte("data: {\"a\":1}\r\n\r\nmore")
		advance, token, err := splitGeminiSSERaw(data, false)
		require.NoError(t, err)
		assert.Equal(t, "data: {\"a\":1}\r\n\r\n", string(token))
		assert.Equal(t, len(token), advance)
	})

	t.Run("incomplete event, not at EOF, requests more data", func(t *testing.T) {
		data := []byte("data: {\"a\":1")
		advance, token, err := splitGeminiSSERaw(data, false)
		require.NoError(t, err)
		assert.Nil(t, token)
		assert.Equal(t, 0, advance)
	})

	t.Run("undelimited final fragment at EOF", func(t *testing.T) {
		data := []byte("data: {\"a\":1}")
		advance, token, err := splitGeminiSSERaw(data, true)
		require.NoError(t, err)
		assert.Equal(t, "data: {\"a\":1}", string(token))
		assert.Equal(t, len(data), advance)
	})

	t.Run("empty data at EOF signals done", func(t *testing.T) {
		advance, token, err := splitGeminiSSERaw(nil, true)
		require.NoError(t, err)
		assert.Nil(t, token)
		assert.Equal(t, 0, advance)
	})
}

// ---------------------------------------------------------------------
// patchGeminiSSEToken
// ---------------------------------------------------------------------

func TestPatchGeminiSSEToken_MissingID(t *testing.T) {
	var counter int
	raw := []byte("data: " + realisticGeminiFunctionCallBody + "\n\n")
	out := patchGeminiSSEToken(raw, &counter)
	require.True(t, bytes.HasSuffix(out, []byte("\n\n")), "delimiter must be preserved exactly")
	require.True(t, bytes.HasPrefix(out, []byte("data:")))

	jsonPart := bytes.TrimSuffix(bytes.TrimPrefix(out, []byte("data:")), []byte("\n\n"))
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(jsonPart, &parsed))
	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 2)
	assert.NotEmpty(t, ids[0])
	assert.NotEmpty(t, ids[1])
	assert.NotEqual(t, ids[0], ids[1])
}

func TestPatchGeminiSSEToken_TextOnlyPassthrough(t *testing.T) {
	var counter int
	raw := []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}],\"role\":\"model\"}}]}\n\n")
	out := patchGeminiSSEToken(raw, &counter)
	assert.Equal(t, raw, out, "an event with no functionCall must be returned byte-for-byte unchanged")
	assert.Equal(t, 0, counter)
}

func TestPatchGeminiSSEToken_NonDataField_Passthrough(t *testing.T) {
	var counter int
	// An SSE "event:" field (or any non-"data" field) containing the literal
	// substring "functionCall" in a comment-like line should not be parsed
	// as JSON at all — fails open.
	raw := []byte("event: functionCall-marker\n\n")
	out := patchGeminiSSEToken(raw, &counter)
	assert.Equal(t, raw, out)
}

func TestPatchGeminiSSEToken_MalformedJSON_Passthrough(t *testing.T) {
	var counter int
	raw := []byte("data: {\"functionCall\": not valid json\n\n")
	out := patchGeminiSSEToken(raw, &counter)
	assert.Equal(t, raw, out)
}

func TestPatchGeminiSSEToken_BlankTokenPassthrough(t *testing.T) {
	var counter int
	raw := []byte("\n\n")
	out := patchGeminiSSEToken(raw, &counter)
	assert.Equal(t, raw, out)
}

func TestPatchGeminiSSEToken_CounterIsSharedAcrossCalls(t *testing.T) {
	// Simulates multiple SSE events each carrying one missing-id
	// functionCall: IDs minted across events must not collide.
	var counter int
	event1 := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":{}}}],"role":"model"}}]}` + "\n\n")
	event2 := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"b","args":{}}}],"role":"model"}}]}` + "\n\n")

	out1 := patchGeminiSSEToken(event1, &counter)
	out2 := patchGeminiSSEToken(event2, &counter)

	id1 := firstFunctionCallID(t, out1)
	id2 := firstFunctionCallID(t, out2)
	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2)
}

func firstFunctionCallID(t *testing.T, sseToken []byte) string {
	t.Helper()
	_, data, found := bytes.Cut(sseToken, []byte(":"))
	require.True(t, found)
	data = bytes.TrimRight(bytes.TrimSpace(data), "\r\n")
	// TrimSpace already removed trailing \n\n; be defensive about \r too.
	data = bytes.TrimSuffix(data, []byte("\r\n\r\n"))
	data = bytes.TrimSuffix(data, []byte("\n\n"))
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &parsed))
	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 1)
	return ids[0]
}

// ---------------------------------------------------------------------
// newGeminiSSEIDPatchingBody / RoundTrip: end-to-end streaming behavior
// ---------------------------------------------------------------------

// slowMultiEventReader emits N pre-built SSE events one at a time, blocking
// on a channel between each event so a test can prove reads are lazy
// (incremental) rather than "read everything, then replay."
type slowMultiEventReader struct {
	events  [][]byte
	release chan struct{}
	mu      sync.Mutex
	sent    int
	reads   int32 // atomic: number of times Read() has been called
	buf     []byte
}

func newSlowMultiEventReader(events [][]byte) *slowMultiEventReader {
	return &slowMultiEventReader{events: events, release: make(chan struct{})}
}

func (r *slowMultiEventReader) Read(p []byte) (int, error) {
	atomic.AddInt32(&r.reads, 1)
	r.mu.Lock()
	if len(r.buf) == 0 {
		if r.sent >= len(r.events) {
			r.mu.Unlock()
			return 0, io.EOF
		}
		// Block until the test explicitly permits emitting the next event —
		// this is what lets the test prove the second event is not read
		// until the first has been consumed by the caller.
		idx := r.sent
		r.mu.Unlock()
		if idx > 0 {
			<-r.release
		}
		r.mu.Lock()
		r.buf = append([]byte{}, r.events[r.sent]...)
		r.sent++
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	r.mu.Unlock()
	return n, nil
}

func (r *slowMultiEventReader) Close() error { return nil }

func (r *slowMultiEventReader) permitNext() { r.release <- struct{}{} }

func TestNewGeminiSSEIDPatchingBody_IsActuallyIncremental(t *testing.T) {
	event1 := []byte(`data: {"candidates":[{"content":{"parts":[{"text":"first"}],"role":"model"}}]}` + "\n\n")
	event2 := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":{}}}],"role":"model"}}]}` + "\n\n")

	src := newSlowMultiEventReader([][]byte{event1, event2})
	body := newGeminiSSEIDPatchingBody(src)
	t.Cleanup(func() { _ = body.Close() })

	// Read the first event out of the patched body.
	got := make([]byte, 0, len(event1))
	buf := make([]byte, 4096)
	deadline := time.After(2 * time.Second)
readFirst:
	for {
		select {
		case <-deadline:
			t.Fatal("timed out reading first event")
		default:
		}
		n, err := body.Read(buf)
		got = append(got, buf[:n]...)
		if bytes.Contains(got, []byte("\n\n")) {
			break readFirst
		}
		if err != nil {
			t.Fatalf("unexpected error/EOF before first event was complete: %v", err)
		}
	}
	assert.Equal(t, string(event1), string(got), "first event must pass through unmodified (no functionCall)")

	// At this point the source's second event must NOT yet have been
	// released/consumed — prove laziness by giving the background goroutine
	// a brief window to (incorrectly) race ahead, then checking sent count.
	src.mu.Lock()
	sentSoFar := src.sent
	src.mu.Unlock()
	assert.Equal(t, 1, sentSoFar, "the second event must not be read from the source until the first has been consumed by the caller")

	// Now allow the second event to be produced and read the rest.
	go src.permitNext()
	rest := make([]byte, 0, 512)
	deadline2 := time.After(2 * time.Second)
readRest:
	for {
		select {
		case <-deadline2:
			t.Fatal("timed out reading second event")
		default:
		}
		n, err := body.Read(buf)
		rest = append(rest, buf[:n]...)
		if err == io.EOF {
			break readRest
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n == 0 {
			continue
		}
	}

	assert.Contains(t, string(rest), `"name":"echo"`)
	assert.True(t, strings.HasSuffix(string(rest), "\n\n"))
	// The functionCall in the second event must have been patched with a
	// non-empty id.
	var parsed map[string]any
	_, data, found := bytes.Cut(rest, []byte(":"))
	require.True(t, found)
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(bytes.TrimSuffix(data, []byte("\n\n"))), &parsed))
	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 1)
	assert.NotEmpty(t, ids[0])
}

// staticReadCloser is a simple io.ReadCloser over a fixed byte slice, used
// where laziness doesn't need to be proven but the SSE splitting/patching
// behavior over multiple real events does.
func staticReadCloser(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}

func TestNewGeminiSSEIDPatchingBody_MultiEventStream_MixedContent(t *testing.T) {
	textEvent := `data: {"candidates":[{"content":{"parts":[{"text":"Let me check that for you.\n"}],"role":"model"}}]}` + "\n\n"
	fcEvent := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_directory","args":{"path":"/tmp"}}},{"functionCall":{"name":"read_file","args":{"path":"/tmp/foo.txt"}}}],"role":"model"}}]}` + "\n\n"
	trailingEvent := `data: {"candidates":[{"finishReason":"STOP"}]}` + "\n\n"

	full := textEvent + fcEvent + trailingEvent
	body := newGeminiSSEIDPatchingBody(staticReadCloser([]byte(full)))
	out, err := io.ReadAll(body)
	require.NoError(t, err)

	outStr := string(out)
	// Text event and trailing event pass through byte-for-byte.
	assert.Contains(t, outStr, textEvent)
	assert.Contains(t, outStr, `"finishReason":"STOP"`)

	// The functionCall event must still be present, with the exact
	// delimiter preserved, and both function calls patched with distinct,
	// non-empty ids.
	require.True(t, strings.HasSuffix(outStr, "\n\n"), "final delimiter must be preserved")

	events := strings.Split(strings.TrimSuffix(outStr, "\n\n"), "\n\n")
	require.Len(t, events, 3)
	var patchedFCEventJSON map[string]any
	_, data, found := strings.Cut(events[1], ":")
	require.True(t, found)
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(data)), &patchedFCEventJSON))
	ids := collectFunctionCallIDs(t, patchedFCEventJSON)
	require.Len(t, ids, 2)
	assert.NotEmpty(t, ids[0])
	assert.NotEmpty(t, ids[1])
	assert.NotEqual(t, ids[0], ids[1])
}

func TestNewGeminiSSEIDPatchingBody_FunctionCallSplitAcrossEvents_NotMidObject(t *testing.T) {
	// Mirrors real Gemini chunking (see gemini_transport.go's streaming-safety
	// comment, confirmed against google.golang.org/genai@v1.68.0's
	// api_client.go): a functionCall is only ever split across *events* in
	// the sense that one call can appear in an earlier event than another,
	// never split *within* a single event's JSON object. This test builds a
	// two-event stream, one functionCall (missing id) per event, and
	// confirms each is patched independently, with its own event boundary
	// intact.
	event1 := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"first_call","args":{"x":1}}}],"role":"model"}}]}` + "\n\n"
	event2 := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"second_call","args":{"y":2}}}],"role":"model"}}]}` + "\n\n"

	body := newGeminiSSEIDPatchingBody(staticReadCloser([]byte(event1 + event2)))
	out, err := io.ReadAll(body)
	require.NoError(t, err)

	events := strings.Split(strings.TrimSuffix(string(out), "\n\n"), "\n\n")
	require.Len(t, events, 2)

	id1 := firstFunctionCallID(t, []byte(events[0]+"\n\n"))
	id2 := firstFunctionCallID(t, []byte(events[1]+"\n\n"))
	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2, "each event's functionCall must get its own distinct synthetic id")
}

func TestNewGeminiSSEIDPatchingBody_CRLFDelimiterPreserved(t *testing.T) {
	fcEvent := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":{}}}],"role":"model"}}]}` + "\r\n\r\n"
	body := newGeminiSSEIDPatchingBody(staticReadCloser([]byte(fcEvent)))
	out, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.True(t, bytes.HasSuffix(out, []byte("\r\n\r\n")), "CRLF delimiter must be preserved exactly, not normalized to LF")
}

// ---------------------------------------------------------------------
// RoundTrip: fail-open behavior, wiring
// ---------------------------------------------------------------------

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func mustRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	require.NoError(t, err)
	return req
}

func TestRoundTrip_NonStreaming_PatchesMissingID(t *testing.T) {
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(200, realisticGeminiFunctionCallBody), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed))
	ids := collectFunctionCallIDs(t, parsed)
	require.Len(t, ids, 2)
	assert.NotEmpty(t, ids[0])
	assert.NotEmpty(t, ids[1])
}

func TestRoundTrip_NonStreaming_AlreadyHasID_Untouched(t *testing.T) {
	original := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","id":"already-set","args":{}}}],"role":"model"}}]}`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(200, original), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, original, string(body))
}

func TestRoundTrip_NonStreaming_NoFunctionCall_Untouched(t *testing.T) {
	original := `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}]}`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(200, original), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, original, string(body))
}

func TestRoundTrip_NonStreaming_MalformedBody_PassesThroughUnmodified(t *testing.T) {
	original := `{"functionCall": not-valid-json-at-all`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(200, original), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, original, string(body))
}

func TestRoundTrip_NonStreaming_NonSuccessStatus_PassesThroughUnmodified(t *testing.T) {
	errorBody := `{"error":{"code":429,"message":"functionCall quota exceeded","status":"RESOURCE_EXHAUSTED"}}`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(429, errorBody), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"))
	require.NoError(t, err)
	assert.Equal(t, 429, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// Note: the error body happens to contain the substring "functionCall"
	// deliberately, to prove the non-2xx short-circuit happens BEFORE any
	// body inspection, not merely because the fast-path substring scan
	// missed it.
	assert.Equal(t, errorBody, string(body))
}

func TestRoundTrip_UnrelatedRequest_PassesThroughUntouched(t *testing.T) {
	original := `{"models":[{"name":"models/gemini-2.5-flash"}]}`
	var bodyWrapped bool
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := newJSONResponse(200, original)
		return resp, nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models"))
	require.NoError(t, err)
	// resp.Body must be the exact same body the base transport returned,
	// never even wrapped — read it and confirm content is untouched.
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, original, string(body))
	assert.False(t, bodyWrapped)
}

func TestRoundTrip_TransportError_PropagatedUnchanged(t *testing.T) {
	wantErr := errors.New("boom: connection refused")
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, wantErr
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"))
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, wantErr)
}

func TestRoundTrip_Streaming_PatchesMissingID(t *testing.T) {
	sseBody := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":{}}}],"role":"model"}}]}` + "\n\n"
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(200, sseBody), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), geminiFunctionCallIDPrefix)
	assert.True(t, strings.HasSuffix(string(body), "\n\n"))
}

func TestRoundTrip_Streaming_NonSuccessStatus_PassesThroughUnmodified(t *testing.T) {
	errorBody := `{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponse(500, errorBody), nil
	})
	transport := newGeminiFunctionCallIDPatchingTransport(base)

	resp, err := transport.RoundTrip(mustRequest(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, errorBody, string(body))
	// resp.Body must not have been swapped for the SSE-patching pipe reader
	// on a non-2xx response — a plain io.ReadAll must succeed synchronously
	// with no goroutine involved.
}

func TestNewGeminiFunctionCallIDPatchingTransport_NilBaseClonesDefaultTransport(t *testing.T) {
	transport := newGeminiFunctionCallIDPatchingTransport(nil)
	require.NotNil(t, transport)
	concrete, ok := transport.(*geminiFunctionCallIDPatchingTransport)
	require.True(t, ok)
	assert.NotNil(t, concrete.base)
}
