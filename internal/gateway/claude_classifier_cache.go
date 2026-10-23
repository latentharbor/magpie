package gateway

import "unicode/utf8"

// A classifier's transcript grows inside one message, not as conversation
// turns. Keep stable text boundaries so another one-off run can read its
// prefix from the cache. The changing tail is left to Claude Code's mark.
const classifierCacheChunk = 64 * 1024

func cacheClaudeClassifier(blocks []map[string]any, instructions int) []map[string]any {
	long := false
	for i, block := range blocks {
		text, _ := block["text"].(string)
		if i > instructions && block["type"] == "text" && len(text) > classifierCacheChunk {
			long = true
			break
		}
	}
	if !long {
		return blocks
	}
	var out []map[string]any
	marked := -1
	for i, block := range blocks {
		text, _ := block["text"].(string)
		if i > instructions && block["type"] == "text" {
			for len(text) > classifierCacheChunk {
				end := classifierCacheChunk
				for !utf8.RuneStart(text[end]) {
					end--
				}
				out = append(out, map[string]any{"type": "text", "text": text[:end]})
				marked = len(out) - 1
				text = text[end:]
			}
		}
		// Copy rather than editing the renderer's maps. This also keeps any
		// image or other non-text block unchanged.
		copy := make(map[string]any, len(block))
		for k, v := range block {
			copy[k] = v
		}
		if block["type"] == "text" {
			copy["text"] = text
		}
		out = append(out, copy)
	}
	if marked < 0 {
		return blocks
	}
	// The default bridge owns only one cache slot. Move it, don't add one;
	// Claude Code owns the others. Client-placed marks never take this path.
	for _, b := range out {
		delete(b, "cache_control")
	}
	out[marked]["cache_control"] = cacheMark1h()
	return out
}
