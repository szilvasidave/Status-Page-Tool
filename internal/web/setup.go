package web

import "net/http"

// TODO: implement (stubs from the skeleton).

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) { s.notImplemented(w, r) }

func (s *Server) handleSetupDatabase(w http.ResponseWriter, r *http.Request) { s.notImplemented(w, r) }

// setupCSRFToken returns the CSRF token used by the setup wizard in setup
// mode (when the auth middleware is not active). TODO: implement.
func setupCSRFToken(r *http.Request) string { return "" }
