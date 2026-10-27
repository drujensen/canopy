package providers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// anthropicToolSchemaMiddleware backfills input_schema on tools that were
// serialized without one. agent-framework-go's anthropicprovider builds
// ToolInputSchemaParam from the tool's "properties"/"required" only, and the
// SDK omits the whole input_schema object when both are empty, so any
// parameterless tool (todo/mode/skill harness tools, MCP tools with no
// arguments) makes the API reject the request with
// "tools.N.custom.input_schema: Field required". This runs below the harness
// middlewares, which add tools at run time, so patching the final request body
// is the only layer that sees every tool.
func anthropicToolSchemaMiddleware(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/messages") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		patched := backfillToolInputSchemas(body)
		req.Body = io.NopCloser(bytes.NewReader(patched))
		req.ContentLength = int64(len(patched))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(patched)), nil }
	}
	return next(req)
}

// backfillToolInputSchemas returns body unchanged when it is not a JSON object
// with a tools array, or when no tool needs patching.
func backfillToolInputSchemas(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	rawTools, ok := payload["tools"]
	if !ok {
		return body
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(rawTools, &tools) != nil {
		return body
	}
	changed := false
	for _, t := range tools {
		if _, has := t["input_schema"]; has {
			continue
		}
		if typ, has := t["type"]; has && string(typ) != `"custom"` {
			continue
		}
		t["input_schema"] = json.RawMessage(`{"type":"object","properties":{}}`)
		changed = true
	}
	if !changed {
		return body
	}
	patchedTools, err := json.Marshal(tools)
	if err != nil {
		return body
	}
	payload["tools"] = patchedTools
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}
