package warp

import (
	"errors"
	"strings"
)

type requestError struct {
	requestID string
	err       error
}

func (e *requestError) Error() string { return e.err.Error() }
func (e *requestError) Unwrap() error { return e.err }
func (e *requestError) WarpRequestID() string {
	return e.requestID
}
func AttachRequestMetadata(err error, _ string, requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if err == nil || requestID == "" {
		return err
	}
	return &requestError{requestID: requestID, err: err}
}

func RequestIDFromError(err error) string {
	var identified interface{ WarpRequestID() string }
	if errors.As(err, &identified) {
		return strings.TrimSpace(identified.WarpRequestID())
	}
	return ""
}
