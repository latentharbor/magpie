package gateway

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Only unsigned assistant thinking from older CLI-backed history is removed.
// Signed (even text-empty) and redacted thinking are opaque and never repaired.
// Work on the outgoing copy, not the client's session or its tool call chain.
func claudeHTTPUnsignedHistory(body []byte) ([]byte, int, error) {
	dropped := 0
	messages := gjson.GetBytes(body, "messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Get("role").String() != "assistant" {
			continue
		}
		blocks := messages[i].Get("content").Array()
		removed := 0
		for j := len(blocks) - 1; j >= 0; j-- {
			b := blocks[j]
			sig := b.Get("signature")
			unsigned := sig.Type == gjson.Null || sig.Type == gjson.String && sig.String() == ""
			if b.Get("type").String() != "thinking" || !unsigned {
				continue
			}
			var err error
			body, err = sjson.DeleteBytes(body, fmt.Sprintf("messages.%d.content.%d", i, j))
			if err != nil {
				return nil, 0, err
			}
			removed++
		}
		if removed != 0 && removed == len(blocks) {
			var err error
			body, err = sjson.DeleteBytes(body, fmt.Sprintf("messages.%d", i))
			if err != nil {
				return nil, 0, err
			}
		}
		dropped += removed
	}
	return body, dropped, nil
}
