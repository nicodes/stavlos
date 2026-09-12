package modelsdev

import _ "embed"

// fallbackJSON is a minimal models.dev snapshot used only when there is
// neither a cache nor network. Prices are USD per million tokens.
//
//go:embed fallback.json
var fallbackJSON []byte
