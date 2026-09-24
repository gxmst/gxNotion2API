package app

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func upstreamRetryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date
	}
	return time.Time{}
}

func credentialBackoff(err error, now time.Time) time.Time {
	var until time.Time
	if isTrustRuleDeniedInferenceError(err) {
		until = now.Add(30 * time.Minute)
	}
	var apiErr *notionAPIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusTooManyRequests && until.IsZero() {
			until = now.Add(2 * time.Minute)
		}
		if !until.IsZero() && apiErr.RetryAfter.After(until) {
			until = apiErr.RetryAfter
		}
	}
	return until
}

func credentialSlotKey(email string) string { return "credential:" + canonicalEmailKey(email) }

// clientInputError marks a failure caused by the request itself -- an
// attachment URL that cannot be fetched, an unsupported attachment type -- rather
// than by the account that happened to serve it. Every account would fail the
// same way, so the dispatcher answers the client with a 400 at once instead of
// refreshing, re-logging or cooling down each candidate in turn.
type clientInputError struct{ err error }

func (e *clientInputError) Error() string {
	if e == nil || e.err == nil {
		return "invalid request input"
	}
	return e.err.Error()
}

func (e *clientInputError) Unwrap() error { return e.err }

func newClientInputError(err error) error {
	if err == nil || isClientInputError(err) {
		return err
	}
	return &clientInputError{err: err}
}

func isClientInputError(err error) bool {
	var target *clientInputError
	return errors.As(err, &target)
}

// clientGoneError wraps a failed write to the client (disconnect, broken pipe).
// It can surface before r.Context() is cancelled, and like a context abort it
// says nothing about the account serving the request.
type clientGoneError struct{ err error }

func (e *clientGoneError) Error() string {
	if e == nil || e.err == nil {
		return "client disconnected"
	}
	return "client disconnected: " + e.err.Error()
}

func (e *clientGoneError) Unwrap() error { return e.err }

func isClientGoneError(err error) bool {
	var target *clientGoneError
	return errors.As(err, &target)
}

// errSessionInvalid is the explicit internal marker for a local session that
// cannot authenticate (no user id, empty cookie jar). Session-invalid detection
// relies on it and on upstream 401/403 status codes, never on free-form text.
var errSessionInvalid = errors.New("notion session invalid")
