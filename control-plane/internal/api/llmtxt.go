package api

import (
	"net/http"
	"os"
)

// handleLLMTxt serves the public API contract (llm.txt) over the
// external path WITHOUT a bearer token — it is intentionally public
// documentation for third-party integrators and coding agents. Served
// from LLMTxtPath on the host (default /etc/sandboxed/llm.txt);
// read per request (the file is tiny) so an edit is reflected without a
// redeploy. 404 when unconfigured or absent. The /llm.txt path is
// listed in the auth middleware's exemptPaths so the external router
// reaches it tokenless.
func (s *Server) handleLLMTxt(w http.ResponseWriter, r *http.Request) {
	if s.LLMTxtPath == "" {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(s.LLMTxtPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}
