package protocol

import (
	"encoding/json"
)

// The wire: JSON-RPC 2.0 framing, one message a line, and the error codes.

// --- JSON-RPC framing ---

type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	V       int              `json:"v"` // protocol version (Version); a daemon refuses a version it does not serve
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
	// Notification fields (when ID is nil).
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
	ErrVersion        = -32000 // unsupported protocol version
	ErrNotFound       = -32001
	ErrConflict       = -32002 // e.g. prompt already claimed, late answer
	ErrTrust          = -32003 // project layer pending trust
	ErrForbidden      = -32004 // the caller may not use the daemon (a process the daemon runs)
)
