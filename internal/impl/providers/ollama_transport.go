package providers

// This file works around a confirmed wire-format bug in Ollama's
// OpenAI-compatible streaming Chat Completions endpoint
// (POST /v1/chat/completions with "stream": true).
//
// Real OpenAI never emits the "content" field at all in a delta object that
// also carries "tool_calls" — a tool-call delta's JSON simply omits
// "content" entirely, it isn't sent as "". Confirmed directly against a live
// Ollama server (ai.drujensen.com, ollama 0.32.13) during this bug's
// investigation, a tool-call delta chunk instead looks like:
//
//	{"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_...",
//	"index":0,"type":"function","function":{"name":"get_weather",
//	"arguments":"{\"location\":\"Paris\"}"}}]},"finish_reason":null}]}
//
// That co-present, empty "content" key breaks the openai-go v3 SDK's
// ChatCompletionAccumulator (streamaccumulator.go, chatCompletionResponseState.update):
// its switch statement checks `delta.JSON.Content.Valid()` (was the field
// *present* in the raw JSON at all, not whether it's non-empty) before
// `delta.JSON.ToolCalls.Valid()`. Since Ollama sends both fields in the same
// delta, the accumulator misclassifies the chunk as a content update instead
// of a tool-call update, so its internal state machine never transitions
// into toolResponseState — which means `ChatCompletionAccumulator.
// JustFinishedToolCall()` never reports the tool call as finished.
//
// agent-framework-go's provider/openaiprovider (chat.go's chatClient.run,
// the streaming branch) relies exclusively on
// `acc.JustFinishedToolCall()` to turn a finished tool call into a
// *message.FunctionCallContent the agent harness ever sees — so with real,
// unpatched Ollama traffic, a tool call is fully and correctly assembled
// inside the accumulator's internal state (Choices[0].Message.ToolCalls) but
// never surfaced to Canopy at all. This was confirmed directly: replaying a
// live streaming response from ai.drujensen.com through openai-go's own
// accumulator, in the exact call pattern openaiprovider uses, shows the tool
// call fully present in the final accumulated state while
// JustFinishedToolCall() never once returns true across the whole stream.
//
// The fix operates one layer below the SDK: an http.RoundTripper installed
// on the *openai.Client Canopy constructs for Ollama (see newOllama)
// rewrites each SSE "data: {...}" event's raw JSON on the way back from the
// server, deleting the "content" key from any choices[].delta object that
// also carries a non-empty "tool_calls" array and whose "content" value is
// the empty string — which is what turns the ambiguous, buggy delta back
// into the shape real OpenAI traffic (and therefore the SDK's accumulator)
// already expects. A "content" value that's non-empty alongside tool_calls
// is left untouched (never observed from Ollama; deleting real streamed
// text on a guess would be worse than leaving this rare shape unpatched).
// Non-streaming (stream:false) responses aren't affected by this bug at all
// — openaiprovider's non-streaming branch reads
// resp.Choices[0].Message.ToolCalls directly off the parsed body, with no
// accumulator involved — so this transport only ever touches
// "text/event-stream" responses and passes everything else through
// completely untouched.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// newOllamaHTTPClient returns an *http.Client for Ollama's OpenAI-compatible
// endpoint with newOllamaToolCallContentStrippingTransport installed. base
// follows impl/tools/web_fetch.go's newSafeTransport precedent (also
// mirrored by gemini_transport.go's newGeminiFunctionCallIDPatchingTransport):
// a nil base clones http.DefaultTransport rather than starting from a bare
// &http.Transport{}, so proxy-from-environment, connection pooling, HTTP/2,
// and TLS defaults all match what a plain http.Client{} would already use.
func newOllamaHTTPClient(base http.RoundTripper) *http.Client {
	return &http.Client{Transport: newOllamaToolCallContentStrippingTransport(base)}
}

func newOllamaToolCallContentStrippingTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &ollamaToolCallContentStrippingTransport{base: base}
}

// ollamaToolCallContentStrippingTransport is the http.RoundTripper installed
// on every *openai.Client Canopy constructs for an Ollama provider (see
// newOllama). It fails open — passing the response through completely
// unmodified — on any non-2xx status, any response that isn't
// "text/event-stream" (the overwhelmingly common case for a non-streaming
// call, and the only shape this bug doesn't affect anyway), and any event
// whose JSON doesn't match the expected shape. A bug in this patching logic
// must never turn a working response into a broken one, or swallow a real
// API error.
type ollamaToolCallContentStrippingTransport struct {
	base http.RoundTripper
}

func (t *ollamaToolCallContentStrippingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		// Fail open: a transport-level error (or a nil response, which
		// shouldn't happen alongside a nil error but is defended against
		// anyway) is not this code's to interpret — propagate it unchanged.
		return resp, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A real API error must reach openai-go's own error handling
		// unmodified — never risk corrupting an error body with
		// shape-specific patching logic that was never written with error
		// responses in mind.
		return resp, nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Not a streaming response — either a non-streaming Chat Completions
		// call (unaffected by this bug, see package comment) or some other
		// endpoint entirely (models listing, etc.). Nothing here to patch.
		return resp, nil
	}
	resp.Body = newOllamaSSEContentStrippingBody(resp.Body)
	return resp, nil
}

// newOllamaSSEContentStrippingBody wraps orig — the raw, still-unparsed SSE
// response body of a streaming Chat Completions call — with an
// io.ReadCloser that re-emits the exact same event framing, patching only
// the events that need it. Mirrors gemini_transport.go's
// newGeminiSSEIDPatchingBody: a goroutine scans orig for the next
// blank-line-delimited event, patches it if needed, and writes it to an
// io.Pipe, so at most one event is ever held in memory ahead of the reader
// and the caller sees events as they actually arrive rather than only after
// the whole stream has finished.
func newOllamaSSEContentStrippingBody(orig io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = orig.Close() }()
		scanner := bufio.NewScanner(orig)
		scanner.Buffer(make([]byte, 1024), 256*1024*1024)
		scanner.Split(splitOllamaSSERaw)

		for scanner.Scan() {
			out := patchOllamaSSEToken(scanner.Bytes())
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

// splitOllamaSSERaw is a bufio.SplitFunc that finds SSE event boundaries
// ("\n\n", then defensively "\r\n\r\n", then whatever's left at EOF) and
// returns each token WITH its original trailing delimiter still attached (no
// CR-stripping), so an event that doesn't need patching can be re-emitted
// with its exact original bytes, delimiter included.
func splitOllamaSSERaw(data []byte, atEOF bool) (advance int, token []byte, err error) {
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

// patchOllamaSSEToken processes one splitOllamaSSERaw token (an SSE event,
// original trailing delimiter still attached). It returns the input
// byte-for-byte unchanged whenever there's nothing to patch or anything
// about the token doesn't match the expected "data:<json>" shape (fail
// open) — only an event that both parses as JSON and actually needs its
// delta's "content" key removed is rebuilt, and even then only its JSON
// payload changes; the event's own "data:" prefix and delimiter bytes are
// preserved exactly as received.
func patchOllamaSSEToken(raw []byte) []byte {
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

	trimmedContent := bytes.TrimSpace(content)
	if len(trimmedContent) == 0 || !bytes.Contains(content, []byte("tool_calls")) {
		return raw
	}

	prefix, data, found := bytes.Cut(content, []byte(":"))
	if !found || string(bytes.TrimSpace(prefix)) != "data" {
		// Not a recognized "data:" event (an SSE comment line, an "event:"
		// field, or the literal "data: [DONE]" sentinel handled implicitly
		// here since it fails the json.Unmarshal below) — pass through
		// exactly as received.
		return raw
	}

	trimmedData := bytes.TrimSpace(data)
	var parsed map[string]any
	if err := json.Unmarshal(trimmedData, &parsed); err != nil {
		return raw
	}
	if !stripEmptyContentAlongsideToolCalls(parsed) {
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

// stripEmptyContentAlongsideToolCalls walks parsed's top-level "choices"
// array (the shape of an OpenAI Chat Completions streaming chunk) and, for
// every choice whose "delta" object carries both a non-empty "tool_calls"
// array and a "content" key whose value is the empty string, deletes the
// "content" key — turning the ambiguous delta back into the shape real
// OpenAI traffic (and the openai-go SDK's accumulator) expects. Returns
// whether anything was changed, so callers can skip a needless re-marshal.
func stripEmptyContentAlongsideToolCalls(parsed map[string]any) bool {
	choicesRaw, ok := parsed["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, choiceRaw := range choicesRaw {
		choice, ok := choiceRaw.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		toolCalls, ok := delta["tool_calls"].([]any)
		if !ok || len(toolCalls) == 0 {
			continue
		}
		content, hasContent := delta["content"]
		if !hasContent {
			continue
		}
		if s, ok := content.(string); ok && s == "" {
			delete(delta, "content")
			changed = true
		}
	}
	return changed
}
