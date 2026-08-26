// Package serviceerror defines transport-neutral errors returned by use cases.
package serviceerror

type Error struct {
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func New(code, message string) *Error { return &Error{Code: code, Message: message} }

func WithDetails(code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}
