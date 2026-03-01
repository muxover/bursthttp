package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
)

// Pre-allocated sentinel errors returned by the library.
// Use IsTimeout and IsRetryable to classify errors without direct comparison.
var (
	ErrConnectFailed     = errors.New("connect failed")
	ErrWriteFailed       = errors.New("write failed")
	ErrReadFailed        = errors.New("read failed")
	ErrHeaderTooLarge    = errors.New("response header too large")
	ErrInvalidResponse   = errors.New("invalid response")
	ErrResponseTooLarge  = errors.New("response too large")
	ErrRequestTooLarge   = errors.New("request body too large")
	ErrHeaderBufferSmall = errors.New("header buffer too small")
	ErrTimeout           = errors.New("operation timeout")
	ErrConnectionClosed  = errors.New("connection closed")
	ErrProxyFailed       = errors.New("proxy connection failed")
	ErrInvalidURL        = errors.New("invalid URL")
	ErrInvalidRequest    = errors.New("invalid request")
)

// ErrorType represents the category of error for logging and handling.
type ErrorType string

const (
	ErrorTypeNetwork    ErrorType = "network"
	ErrorTypeTimeout    ErrorType = "timeout"
	ErrorTypeProtocol   ErrorType = "protocol"
	ErrorTypeTLS        ErrorType = "tls"
	ErrorTypeProxy      ErrorType = "proxy"
	ErrorTypeValidation ErrorType = "validation"
	ErrorTypeInternal   ErrorType = "internal"
)

// DetailedError wraps an error with type and context for detailed logging.
type DetailedError struct {
	Type    ErrorType
	Message string
	Err     error
	Context map[string]interface{}
}

// Error implements the error interface.
func (e *DetailedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Type, e.Message, e.Err)
	}
	return fmt.Sprintf("[%s] %s", e.Type, e.Message)
}

// Unwrap returns the underlying error.
func (e *DetailedError) Unwrap() error {
	return e.Err
}

// LogErrorWithFlag logs a detailed error with type and context if enableLogging is true.
func LogErrorWithFlag(errType ErrorType, message string, err error, context map[string]interface{}, enableLogging bool) error {
	detailedErr := &DetailedError{
		Type:    errType,
		Message: message,
		Err:     err,
		Context: context,
	}

	if enableLogging {
		if len(context) > 0 {
			log.Printf("ERROR [%s] %s: %v | Context: %+v", errType, message, err, context)
		} else {
			log.Printf("ERROR [%s] %s: %v", errType, message, err)
		}
	}

	return detailedErr
}

// WrapError creates a detailed error without logging (for retry scenarios).
func WrapError(errType ErrorType, message string, err error) error {
	return &DetailedError{
		Type:    errType,
		Message: message,
		Err:     err,
	}
}

// IsTimeout checks if an error is a timeout error.
// It handles net.Error timeouts, context.DeadlineExceeded, ErrTimeout,
// and DetailedError with ErrorTypeTimeout — including wrapped errors.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var detailedErr *DetailedError
	if errors.As(err, &detailedErr) && detailedErr.Type == ErrorTypeTimeout {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// IsRetryable checks if an error is retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrConnectFailed) || errors.Is(err, ErrWriteFailed) ||
		errors.Is(err, ErrReadFailed) || errors.Is(err, ErrConnectionClosed) ||
		errors.Is(err, ErrTimeout) {
		return true
	}
	var detailedErr *DetailedError
	if errors.As(err, &detailedErr) {
		return detailedErr.Type == ErrorTypeNetwork || detailedErr.Type == ErrorTypeTimeout
	}
	return false
}
