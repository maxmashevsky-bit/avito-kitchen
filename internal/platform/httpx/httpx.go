// Package httpx contains process-independent HTTP helpers and middleware.
package httpx

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const requestIDKey contextKey = "request-id"

type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	appErr := &Error{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "internal server error"}
	var candidate *Error
	if errors.As(err, &candidate) {
		appErr = candidate
	}
	body := map[string]any{
		"code":       appErr.Code,
		"message":    appErr.Message,
		"request_id": RequestID(r.Context()),
	}
	if len(appErr.Details) > 0 {
		body["details"] = appErr.Details
	}
	WriteJSON(w, appErr.Status, body)
}

func DecodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return NewError(http.StatusRequestEntityTooLarge, "VALIDATION_ERROR", "request body is too large")
		}
		return NewError(http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return NewError(http.StatusBadRequest, "VALIDATION_ERROR", "request body must contain one JSON value")
	}
	return nil
}

func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" || len(requestID) > 128 {
				requestID = uuid.NewString()
			}
			w.Header().Set("X-Request-ID", requestID)
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("http panic", "request_id", requestID, "panic", recovered, "stack", string(debug.Stack()))
					WriteError(w, r.WithContext(ctx), errors.New("panic"))
				}
				logger.Info("http request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
			}()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func BearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return "", false
	}
	return value[len(prefix):], true
}

func SecureEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
