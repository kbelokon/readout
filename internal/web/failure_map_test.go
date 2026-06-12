package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"

	"github.com/kbelokon/readout/internal/kube"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// failureRow is one frozen row of the cross-path failure-classification
// contract: a fabricated upstream error and the observable output each
// presentation path must produce for it. The contract freezes EACH path
// separately (per-path determinism, not cross-path equality): a deadline shows
// a "timeout" search chip but the generic unreachable list state, and that split
// is intentional.
type failureRow struct {
	name string
	err  error

	searchChip string // searchScopeReason output

	// listState is the list/detail whole-failure state the error resolves to:
	// "forbidden" -> stateForbidden card, "unreachable" -> stateUnreachable card,
	// "" -> no state card (the caller falls through to its existing handling).
	listState string

	streamStatus int // initial-list handshake HTTP status
}

func apiStatus(code int32, reason metav1.StatusReason, msg string) error {
	return &kerrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Reason:  reason,
		Code:    code,
		Message: msg,
	}}
}

// failureRows is the frozen baseline table. Every row is the same upstream
// failure observed through every presentation path; the bytes here are the
// user-visible contract and must not drift.
func failureRows() []failureRow {
	return []failureRow{
		{
			name:         "apiserver 403",
			err:          kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", nil),
			searchChip:   "forbidden",
			listState:    "forbidden",
			streamStatus: http.StatusForbidden,
		},
		{
			name:         "apiserver 401",
			err:          apiStatus(http.StatusUnauthorized, metav1.StatusReasonUnauthorized, "Unauthorized"),
			searchChip:   "failed",
			listState:    "forbidden",
			streamStatus: http.StatusForbidden,
		},
		{
			name:         "apiserver 404",
			err:          kerrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x"),
			searchChip:   "failed",
			listState:    "",
			streamStatus: http.StatusNotFound,
		},
		{
			name:         "apiserver 500",
			err:          apiStatus(http.StatusInternalServerError, metav1.StatusReasonInternalError, "Internal error occurred: boom"),
			searchChip:   "failed",
			listState:    "unreachable",
			streamStatus: http.StatusBadGateway,
		},
		{
			name:         "apiserver 429",
			err:          apiStatus(http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "too many requests"),
			searchChip:   "failed",
			listState:    "",
			streamStatus: http.StatusBadGateway,
		},
		{
			name:         "connection refused",
			err:          &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
			searchChip:   "unreachable",
			listState:    "unreachable",
			streamStatus: http.StatusBadGateway,
		},
		{
			name:         "context deadline exceeded",
			err:          context.DeadlineExceeded,
			searchChip:   "timeout",
			listState:    "unreachable",
			streamStatus: http.StatusBadGateway,
		},
		{
			name:         "plain opaque error",
			err:          errOpaque,
			searchChip:   "failed",
			listState:    "unreachable",
			streamStatus: http.StatusBadGateway,
		},
	}
}

// errOpaque is a plain error with no apiserver Status, no net.Error, no syscall
// underneath -- the unrecognized-error case the taxonomy folds into "internal".
var errOpaque = &opaqueError{}

type opaqueError struct{}

func (*opaqueError) Error() string { return "something went wrong" }

// observedListState runs the error through the real detail/list state selection
// and reports the resolved state as "forbidden" / "unreachable" / "" (no card).
// detailState shares its forbidden/unreachable/no-state boolean split with
// buildListState, so it is the honest seam for the list-state row.
func observedListState(t *testing.T, err error) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/clusters/c/namespaces/default/pods/x", nil)
	v := (&Server{}).detailState(req, &kube.Cluster{Name: "c"}, "pods", "x", "default", "get", err)
	if v == nil || v.State == nil {
		return ""
	}
	switch v.State.Kind {
	case stateForbidden:
		return "forbidden"
	case stateUnreachable:
		return "unreachable"
	default:
		t.Fatalf("unexpected state kind %v", v.State.Kind)
		return ""
	}
}

// TestFailureClassificationContract is the two-phase consistency contract: it
// fabricates each frozen row and asserts the search chip, the list/detail state,
// and the stream handshake status the error resolves to. It must pass against the
// pre-refactor implementation (validating the table) and against the unified
// classifier (proving the swap is byte-identical).
func TestFailureClassificationContract(t *testing.T) {
	for _, row := range failureRows() {
		t.Run(row.name, func(t *testing.T) {
			if got := searchScopeReason([]searchErrorRecord{{err: row.err}}); got != row.searchChip {
				t.Errorf("search chip = %q, want %q", got, row.searchChip)
			}
			if got := observedListState(t, row.err); got != row.listState {
				t.Errorf("list/detail state = %q, want %q", got, row.listState)
			}
			if got := streamHandshakeStatus(row.err); got != row.streamStatus {
				t.Errorf("stream handshake status = %d, want %d", got, row.streamStatus)
			}
		})
	}
}
