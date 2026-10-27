package providers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// openaiResponsesHistoryMiddleware makes Responses API replay tolerant of
// history Canopy has reloaded, compacted or edited. With store=false the
// framework replays prior reasoning as encrypted_content, but it recovers the
// reasoning item's required id from an in-memory RawRepresentation that is not
// persisted (json:"-"), and the API also rejects a reasoning item that is not
// followed by the output it produced. Compaction or a manual edit can strand
// one the same way. Reasoning items that cannot be replayed validly are dropped
// from the request, so history stays Canopy's to rewrite and the worst case is
// losing reasoning continuity rather than a 400.
func openaiResponsesHistoryMiddleware(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/responses") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		patched := pruneUnreplayableReasoning(body)
		req.Body = io.NopCloser(bytes.NewReader(patched))
		req.ContentLength = int64(len(patched))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(patched)), nil }
	}
	return next(req)
}

// pruneUnreplayableReasoning returns body unchanged when it isn't a JSON object
// with an input array or nothing needs dropping.
func pruneUnreplayableReasoning(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	rawInput, ok := payload["input"]
	if !ok {
		return body
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(rawInput, &items) != nil {
		return body
	}

	kept := make([]map[string]json.RawMessage, 0, len(items))
	for i, it := range items {
		if itemString(it, "type") == "reasoning" && !reasoningReplayable(items, i) {
			continue
		}
		kept = append(kept, it)
	}
	if len(kept) == len(items) {
		return body
	}

	input, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	payload["input"] = input
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// reasoningReplayable reports whether items[i], a reasoning item, has the id
// the API requires and is followed by something the model produced from it: a
// function call, assistant message, or further reasoning.
func reasoningReplayable(items []map[string]json.RawMessage, i int) bool {
	if itemString(items[i], "id") == "" || i+1 >= len(items) {
		return false
	}
	next := items[i+1]
	switch itemString(next, "type") {
	case "function_call", "reasoning":
		return true
	case "function_call_output":
		return false
	}
	return itemString(next, "role") == "assistant"
}

func itemString(it map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := it[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}
