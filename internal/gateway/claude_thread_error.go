package gateway

import (
	"strings"

	"github.com/tidwall/gjson"
)

// The native Code client owns the full history, including fields omitted from
// Thread continuations. Preserve the upstream replay signal rather than trying
// a delta against another account, or inventing a new Thread from partial data.
// Only documented-by-client protocol codes are copied, never arbitrary details.
func claudeThreadErrorCode(status int, body []byte) string {
	code := gjson.GetBytes(body, "error.details.error_code").String()
	switch code {
	case "thread_not_found":
		if status == 404 {
			return code
		}
	case "thread_fingerprint_mismatch", "thread_already_continued", "thread_unsupported_request":
		if status == 400 {
			return code
		}
	}
	// Some upstream versions only send the explicit full-replay instruction.
	// Code's 404 classifier still needs the machine-readable thread code.
	if status == 404 && code == "" {
		msg := gjson.GetBytes(body, "error.message").String()
		if strings.Contains(msg, "No thread state was found for the requested `previous_message_id`") && strings.Contains(msg, "Replay the full conversation") && strings.Contains(msg, `"type": "create"`) {
			return "thread_not_found"
		}
	}
	return ""
}

func claudeThreadErrorBody(status int, msg, code string) any {
	typ := "invalid_request_error"
	if status == 404 {
		typ = "not_found_error"
	}
	return map[string]any{"type": "error", "error": map[string]any{
		"type": typ, "message": msg, "details": map[string]string{"error_code": code},
	}}
}
