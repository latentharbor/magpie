package usage

// ClaudeHTTPDiagnostic describes the final outgoing HTTP subscription request.
// Hashes are keyed per process: comparable only until restart, not prompt text
// or stable identifiers for correlating a user's content across installations.
type ClaudeHTTPDiagnostic struct {
	RunID           string              `json:"run_id"`
	Native          bool                `json:"native"`
	MaxTokens       int64               `json:"max_tokens"`
	Messages        int                 `json:"messages"`
	Tools           int                 `json:"tools"`
	RequestClass    string              `json:"request_class"`
	CacheSource     string              `json:"cache_source"`
	UnsignedDropped int                 `json:"unsigned_thinking_dropped,omitempty"`
	SystemHash      string              `json:"system_hash"`
	ToolsHash       string              `json:"tools_hash"`
	ConfigHash      string              `json:"config_hash"`
	Prefixes        []ClaudeCachePrefix `json:"prefixes,omitempty"`
	PrefixesOmitted int                 `json:"prefixes_omitted,omitempty"`
}

type ClaudeCachePrefix struct {
	Position string `json:"position"`
	TTL      string `json:"ttl"`
	Hash     string `json:"hash"`
}
