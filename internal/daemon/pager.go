package daemon

import (
	"errors"
	"net/http"

	"github.com/dialohq/roamvm/internal/state"
)

// The runner receives verified checkpoint chunks over its private Pod socket,
// never object-store credentials or access to unrelated VMs' chunks.
func (s *Server) chunk(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var err error
	if s.readChunk == nil {
		var p Prepared
		p, err = s.load(s.PodUID)
		if err == nil && p.Session.Head.Checkpoint == nil {
			err = state.ErrNotFound
		}
		if err == nil {
			s.readChunk, err = s.State.ChunkReader(r.Context(), p.Session)
		}
	}
	read := s.readChunk
	s.mu.Unlock()
	var b []byte
	if err == nil {
		b, err = read(r.Context(), r.URL.Query().Get("key"))
	}
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, state.ErrNotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}
