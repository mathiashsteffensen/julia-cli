package provider

import (
	"encoding/json"
	"errors"
	"strings"

	"charm.land/fantasy"
)

// maxUpstreamDetail bounds an upstream reason quoted in an error message.
const maxUpstreamDetail = 500

// upstreamError adds the upstream provider's own reason to a gateway error
// whose message alone does not say what went wrong.
type upstreamError struct {
	err    error
	detail string
}

func (e *upstreamError) Error() string { return e.err.Error() + " (" + e.detail + ")" }
func (e *upstreamError) Unwrap() error { return e.err }

// explainUpstream returns err with the reason a gateway relayed from the
// upstream provider, or err unchanged when there is none. OpenRouter answers a
// rejected request with a generic "Provider returned error" and keeps the
// provider's reply in error.metadata.raw.
func explainUpstream(err error) error {
	if detail := upstreamDetail(err); detail != "" {
		return &upstreamError{err: err, detail: detail}
	}
	return err
}

func upstreamDetail(err error) string {
	// The SDK error carries the decoded error object; a mid-stream error event
	// carries the whole envelope in the response body.
	var raw string
	var sdkErr interface{ RawJSON() string }
	var providerErr *fantasy.ProviderError
	if errors.As(err, &sdkErr) {
		raw = sdkErr.RawJSON()
	} else if errors.As(err, &providerErr) && json.Valid(providerErr.ResponseBody) {
		raw = string(providerErr.ResponseBody)
	}
	var gateway struct {
		Error    *json.RawMessage `json:"error"`
		Metadata struct {
			Raw          json.RawMessage `json:"raw"`
			ProviderName string          `json:"provider_name"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(raw), &gateway) != nil {
		return ""
	}
	if gateway.Error != nil {
		if json.Unmarshal(*gateway.Error, &gateway) != nil {
			return ""
		}
	}
	// metadata.raw is usually the provider's reply as a string, sometimes an object.
	var reply string
	if json.Unmarshal(gateway.Metadata.Raw, &reply) != nil {
		reply = string(gateway.Metadata.Raw)
	}
	detail := replyMessage(strings.TrimSpace(reply))
	if detail == "" {
		return ""
	}
	if len(detail) > maxUpstreamDetail {
		detail = strings.ToValidUTF8(detail[:maxUpstreamDetail], "") + "…"
	}
	if gateway.Metadata.ProviderName != "" {
		detail = gateway.Metadata.ProviderName + ": " + detail
	}
	return detail
}

// replyMessage picks the message out of a provider's error reply in the shapes
// providers commonly use, falling back to the reply itself.
func replyMessage(reply string) string {
	var shapes struct {
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(reply), &shapes) != nil {
		return reply
	}
	if shapes.Message != "" {
		return shapes.Message
	}
	for _, nested := range []json.RawMessage{shapes.Error, shapes.Detail} {
		var text string
		if json.Unmarshal(nested, &text) == nil && text != "" {
			return text
		}
		var object struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(nested, &object) == nil && object.Message != "" {
			return object.Message
		}
	}
	return reply
}
