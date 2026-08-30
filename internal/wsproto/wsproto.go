// Package wsproto is the JSON contract for the local unix socket between the `agentparley ws` subcommand (the
// client) and the running daemon (the server): one Request in, one Response out, per connection. Both sides import
// this so the shape has a single owner and cannot drift.
package wsproto

// Request is the one JSON object a `ws` invocation writes to the socket.
type Request struct {
	Op            string `json:"op"` // "read" | "writeBegin" | "writeCommit"
	SessionID     int64  `json:"sessionId"`
	AgentSelector string `json:"agentSelector"`
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	UploadToken   int64  `json:"uploadToken"`
	ContentHash   string `json:"contentHash"`
}

// Response is the one JSON object the daemon writes back.
type Response struct {
	OK           bool              `json:"ok"`
	URL          string            `json:"url,omitempty"`
	Fields       map[string]string `json:"fields,omitempty"`
	UploadToken  int64             `json:"uploadToken,omitempty"`
	DisplayPath  string            `json:"displayPath,omitempty"`
	SizeBytes    int64             `json:"sizeBytes,omitempty"`
	ErrorCode    string            `json:"errorCode,omitempty"`
	ErrorMessage string            `json:"errorMessage,omitempty"`
}
