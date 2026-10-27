package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackfillToolInputSchemas(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"adds schema to parameterless tool",
			`{"model":"m","tools":[{"name":"a"},{"name":"b","input_schema":{"type":"object","properties":{"x":{}}}}]}`,
			`{"model":"m","tools":[{"input_schema":{"type":"object","properties":{}},"name":"a"},{"input_schema":{"type":"object","properties":{"x":{}}},"name":"b"}]}`,
		},
		{"leaves server tools alone", `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
		{"no tools", `{"model":"m"}`, `{"model":"m"}`},
		{"not json", `nope`, `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(backfillToolInputSchemas([]byte(tt.in)))
			if json.Valid([]byte(tt.want)) {
				assert.JSONEq(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestAnthropicToolSchemaMiddleware_PatchesRequestBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"tools":[{"name":"a"}]}`))
	require.NoError(t, err)
	resp, err := anthropicToolSchemaMiddleware(req, http.DefaultClient.Do)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Contains(t, got, `"input_schema":{"type":"object","properties":{}}`)
}
