package grok

import "net/http"

// signerProbePath reports whether a request is the statsig signer reading the
// account's own page before signing. Tests that assert the exact upstream request
// a client makes answer the probe and ignore it, so the assertion keeps describing
// the request under test rather than every request the client happens to emit.
func signerProbePath(path string) bool {
	return path == "/index" || path == "/"
}

// answerSignerProbe ends a statsig page read with a plain 404: the signer treats
// that as "no signature available" and the request continues without the header,
// which is what these tests expect.
func answerSignerProbe(w http.ResponseWriter, r *http.Request) {
	_ = r
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found"))
}
