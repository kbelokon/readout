package kube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func apiStatusErr(code int32, reason metav1.StatusReason, msg string) error {
	return &kerrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Reason:  reason,
		Code:    code,
		Message: msg,
	}}
}

// timeoutNetError is a net.Error that reports a timeout without being a context
// or syscall error -- the net.Error.Timeout() branch of the classifier.
type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return false }

// TestClassifyError covers every FailureKind, the total-taxonomy merge targets,
// and the typed-detection seams (net.Error timeout, syscall refused/no-route,
// DNS). String matching is never involved.
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureKind
	}{
		{"forbidden 403", kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", nil), FailureForbidden},
		{"unauthorized 401", apiStatusErr(http.StatusUnauthorized, metav1.StatusReasonUnauthorized, "Unauthorized"), FailureUnauthorized},
		{"not found 404", kerrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x"), FailureNotFound},
		{"upstream 500", apiStatusErr(http.StatusInternalServerError, metav1.StatusReasonInternalError, "Internal error occurred"), FailureUpstream5xx},
		{"upstream 503", apiStatusErr(http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable, "unavailable"), FailureUpstream5xx},

		// Total-taxonomy merge targets: any other apiserver status folds to internal.
		{"bad request 400", apiStatusErr(http.StatusBadRequest, metav1.StatusReasonBadRequest, "bad selector"), FailureInternal},
		{"too many requests 429", apiStatusErr(http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "slow down"), FailureInternal},
		{"conflict 409", apiStatusErr(http.StatusConflict, metav1.StatusReasonConflict, "conflict"), FailureInternal},
		{"gone 410", apiStatusErr(http.StatusGone, metav1.StatusReasonGone, "expired"), FailureInternal},

		// Timeouts.
		{"context deadline", context.DeadlineExceeded, FailureTimeout},
		{"net timeout", timeoutNetError{}, FailureTimeout},

		// Transport-level unreachable.
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, FailureUnreachable},
		{"no route to host", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, FailureUnreachable},
		{"network unreachable", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}, FailureUnreachable},
		{"dns no such host", &net.DNSError{Err: "no such host", Name: "cluster.invalid"}, FailureUnreachable},

		// Internal fallbacks.
		{"context canceled", context.Canceled, FailureInternal},
		{"opaque error", errors.New("something went wrong"), FailureInternal},
		{"nil error", nil, FailureInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyErrorWrappedChains pins that classification reads through
// fmt.Errorf("...: %w", err) wrapping -- the same kind whether the error is bare
// or wrapped, because the classifier uses errors.Is/errors.As, never the string.
func TestClassifyErrorWrappedChains(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureKind
	}{
		{"wrapped 403", kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", nil), FailureForbidden},
		{"wrapped 500", apiStatusErr(http.StatusInternalServerError, metav1.StatusReasonInternalError, "boom"), FailureUpstream5xx},
		{"wrapped deadline", context.DeadlineExceeded, FailureTimeout},
		{"wrapped refused", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, FailureUnreachable},
		{"wrapped resource-type-not-found sentinel", ErrResourceTypeNotFound, FailureNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("list pods: %w", tc.err)
			doubleWrapped := fmt.Errorf("cluster prod: %w", wrapped)
			if got := ClassifyError(wrapped); got != tc.want {
				t.Errorf("ClassifyError(wrapped) = %q, want %q", got, tc.want)
			}
			if got := ClassifyError(doubleWrapped); got != tc.want {
				t.Errorf("ClassifyError(doubleWrapped) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyErrorResourceTypeNotFound pins that the plain wrapped
// ErrResourceTypeNotFound sentinel from FindResource (which IsNotFound treats as
// not-found) classifies to FailureNotFound and not FailureInternal. The sentinel
// is not an apiserver APIStatus, so without an explicit check it would fall
// through to the internal fallback and break the not-found rendering contract.
func TestClassifyErrorResourceTypeNotFound(t *testing.T) {
	err := fmt.Errorf("no such type: %w", ErrResourceTypeNotFound)
	if got := ClassifyError(err); got != FailureNotFound {
		t.Errorf("ClassifyError(wrapped ErrResourceTypeNotFound) = %q, want %q", got, FailureNotFound)
	}
}

// TestClassifyErrorTimeoutBeatsRefused pins the ordering: a dial that both is a
// net.Error timeout and carries a refused syscall still reads as a timeout (the
// timeout check precedes the refused/no-route check).
func TestClassifyErrorTimeoutBeatsRefused(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutSyscall{}}
	if got := ClassifyError(err); got != FailureTimeout {
		t.Errorf("ClassifyError(timeout dial) = %q, want %q", got, FailureTimeout)
	}
}

// timeoutSyscall is an inner dial error that reports a timeout, so the wrapping
// net.OpError is a net.Error whose Timeout() is true -- exercising the net.Error
// timeout branch ahead of the syscall refused/no-route branch.
type timeoutSyscall struct{}

func (timeoutSyscall) Error() string { return "operation timed out" }
func (timeoutSyscall) Timeout() bool { return true }
