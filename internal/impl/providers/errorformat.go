package providers

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

// FormatError renders err for display to a user (the TUI's status-error
// line, chat.go's renderStatusErrLine) as "<http-status> - <message>" when
// err is (or wraps, via errors.As) a recognized provider SDK's structured
// API error — instead of that SDK's own Error(), which buries the one
// line a user actually needs (the server's own human-readable message)
// under a method/URL/raw-JSON-body prefix. That prefix is routinely long
// enough that it's exactly what survives renderStatusErrLine's
// single-line width clamp while the actual message gets truncated off the
// end — the real-incident this was added for: OpenCode's 403 body
// ("An active OpenCode Go subscription is required to use Go models.")
// was there the whole time, just never visible.
//
// Falls back to err.Error() unchanged for anything this doesn't
// recognize (a plain Go error, context.Canceled, an SDK error shape not
// listed here), so no existing caller's display regresses for an error
// this can't improve.
func FormatError(err error) string {
	if err == nil {
		return ""
	}

	var openaiErr *openai.Error
	if errors.As(err, &openaiErr) && openaiErr.Message != "" {
		return fmt.Sprintf("%d - %s", openaiErr.StatusCode, openaiErr.Message)
	}

	var genaiErr *genai.APIError
	if errors.As(err, &genaiErr) && genaiErr.Message != "" {
		return fmt.Sprintf("%d - %s", genaiErr.Code, genaiErr.Message)
	}

	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) {
		if msg := anthropicErrorMessage(anthropicErr); msg != "" {
			return fmt.Sprintf("%d - %s", anthropicErr.StatusCode, msg)
		}
	}

	return err.Error()
}

// anthropicErrorMessage extracts the "message" field anthropic-sdk-go's
// own *anthropic.Error doesn't parse out itself — unlike openai.Error and
// genai.APIError, both of which already unmarshal a Message field,
// anthropic.Error only exposes StatusCode/RequestID/RawJSON(). Its
// RawJSON() is the full response body, shaped
// {"type":"error","error":{"type":"...","message":"..."}}, so this reaches
// one level deeper than the other two needed to. Returns "" (not an error)
// on anything unparseable, since this is a best-effort display
// enhancement, not something that should ever fail a turn.
func anthropicErrorMessage(aerr *anthropic.Error) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(aerr.RawJSON()), &envelope) != nil {
		return ""
	}
	return envelope.Error.Message
}
