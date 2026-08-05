package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A device-code poll cannot treat every failure the same way. The RFC 8628
// states — authorization_pending, slow_down, access_denied, expired_token —
// arrive as a normal error body with a non-2xx status, and each means something
// different to the loop. A transport failure looks nothing like them and must
// stop the loop rather than being read as "the user has not approved yet".
//
// This type is what lets a caller tell those apart.

// oauthErrorBody is an RFC 6749 §5.2 error response.
type oauthErrorBody struct {
	Status int
	// Code is the RFC 6749 error identifier — the field the poll loop
	// branches on. It is not named Error because that is the error interface.
	Code             string  `json:"error"`
	ErrorDescription string  `json:"error_description"`
	Message          string  `json:"message"`
	Interval         float64 `json:"interval"`
	// Body is the raw response, kept for the case where nothing parsed.
	Body string
}

func (e *oauthErrorBody) detail() string {
	parts := make([]string, 0, 2)
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.ErrorDescription != "" {
		parts = append(parts, e.ErrorDescription)
	} else if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 {
		if trimmed := strings.TrimSpace(e.Body); trimmed != "" {
			return fmt.Sprintf("HTTP %d: %s", e.Status, trimmed)
		}
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return strings.Join(parts, ": ")
}

func (e *oauthErrorBody) Error() string { return e.detail() }

// newOAuthError builds an error from a failed token or device-code response.
// An unparseable body still produces one, carrying the status, because a
// gateway's HTML error page is exactly when the status is all there is.
func newOAuthError(status int, body []byte) *oauthErrorBody {
	out := &oauthErrorBody{Status: status, Body: string(body)}
	_ = json.Unmarshal(body, out)
	return out
}

// asOAuthError reports whether err is a provider error response.
func asOAuthError(err error, target **oauthErrorBody) bool {
	return errors.As(err, target)
}
