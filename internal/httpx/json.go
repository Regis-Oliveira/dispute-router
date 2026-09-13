package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON encodes body as the whole response at the given status.
//
// The encoder's own failure is ignored on purpose: the status line is already
// on the wire by then, so there is nothing left to say to the client.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError writes {"error": message} at the given status.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}
