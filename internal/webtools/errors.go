// Package webtools serves the /v1/tools endpoints that let approved local
// apps search the web (through a configured search provider) and read web
// pages through this machine. See docs/web-tools.md in CLI Tollgate for the
// client contract.
package webtools

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Error codes returned in {"error": {"code": ...}}.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeProviderUnavailable = "provider_unavailable"
	CodeInsecureURL         = "insecure_url"
	CodeScopeRequired       = "scope_required"
	CodeBlockedDomain       = "blocked_domain"
	CodePrivateAddress      = "private_address"
	CodeTooManyRedirects    = "too_many_redirects"
	CodeTooLarge            = "too_large"
	CodeUnsupportedType     = "unsupported_type"
	CodeNoContent           = "no_content"
	CodeFetchFailed         = "fetch_failed"
	CodeSearchFailed        = "search_failed"
	CodeDisabled            = "disabled"
	CodeNotConfigured       = "not_configured"
	CodeTimeout             = "timeout"
	CodeRateLimited         = "rate_limited"
)

// toolError is a refusal or failure with its HTTP status and code.
type toolError struct {
	HTTPStatus int
	Code       string
	Message    string
	// UpstreamStatus is the site's or provider's HTTP status, when there was one.
	UpstreamStatus int
	// RetryAfter is set for rate_limited, in seconds.
	RetryAfter int
}

func (e *toolError) Error() string { return e.Code + ": " + e.Message }

func newError(status int, code, format string, args ...any) *toolError {
	return &toolError{HTTPStatus: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func errInvalid(format string, args ...any) *toolError {
	return newError(http.StatusBadRequest, CodeInvalidRequest, format, args...)
}

func errInsecure(format string, args ...any) *toolError {
	return newError(http.StatusBadRequest, CodeInsecureURL, format, args...)
}

func errBlocked(host string) *toolError {
	return newError(http.StatusForbidden, CodeBlockedDomain, "%s is blocked in Tollgate's settings", host)
}

func errPrivate(host string) *toolError {
	return newError(http.StatusForbidden, CodePrivateAddress, "%s is a private or local address", host)
}

func errTimeout(what string) *toolError {
	return newError(http.StatusGatewayTimeout, CodeTimeout, "%s took longer than the time limit", what)
}

// writeError renders the error envelope shared by every /v1/tools route.
func writeError(c *gin.Context, err *toolError) {
	body := gin.H{"code": err.Code, "message": err.Message}
	if err.UpstreamStatus != 0 {
		body["status"] = err.UpstreamStatus
	}
	if err.RetryAfter > 0 {
		body["retry_after"] = err.RetryAfter
		c.Header("Retry-After", strconv.Itoa(err.RetryAfter))
	}
	c.JSON(err.HTTPStatus, gin.H{"error": body})
}
