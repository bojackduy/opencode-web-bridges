package bridgecore

import (
	"encoding/json"
	"net/http"
)

// WriteSSEChunk writes one SSE data frame.
func WriteSSEChunk(w http.ResponseWriter, v any) {
	raw, _ := json.Marshal(v)
	_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// WriteCompletion writes a non-streaming JSON completion.
func WriteCompletion(w http.ResponseWriter, resp any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// WriteDone writes the terminal SSE DONE frame.
func WriteDone(w http.ResponseWriter) {
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}
