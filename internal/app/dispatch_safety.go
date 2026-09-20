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
