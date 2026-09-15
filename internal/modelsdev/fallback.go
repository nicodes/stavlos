package modelsdev

import _ "embed"

// fallbackJSON is a trimmed models.dev snapshot (the gpt-5 family under
// openai and grok under xai, ids, names and limits only) used when there is
// neither a cache nor network; a background refresh replaces it.
//
//go:embed fallback.json
var fallbackJSON []byte
