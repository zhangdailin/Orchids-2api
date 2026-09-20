package handler

import (
	"net/http"

	apperrors "orchids-api/internal/errors"
)

// writePoolExhaustion answers a request the pool could not take at all. It is the
// initial-selection entrance's answer, where nothing has been committed yet.
//
// The classification itself lives in internal/errors: every provider handler and
// both generic entrances must answer the same pool the same way. This wrapper
// only supplies the residual default for an entrance that fails before any
// upstream call — the pool's note is in the log (see the caller), and a client
// can do nothing about "there are no usable accounts", so the answer points at
// the operator.
func writePoolExhaustion(w http.ResponseWriter, out apperrors.PoolExhaustion) {
	if out.Empty() {
		out = apperrors.PoolExhaustion{Category: "configuration", Message: apperrors.PoolNoAccountsMessage}
	}
	apperrors.New(out.Category, out.Message, apperrors.StatusForCategory(out.Category)).WriteResponse(w)
}

// classifyPoolExhaustion is the generic entrances' name for the shared rule in
// internal/errors, so the session handler and the streaming entrance read the
// way they did when the rule lived here.
func classifyPoolExhaustion(selectErr error, lastErr string) apperrors.PoolExhaustion {
	return apperrors.ClassifyPoolExhaustion(selectErr, lastErr)
}
