package web

import (
	"net/http"

	"github.com/kbelokon/readout/internal/kube"
)

// failure_map.go is the single web-side mapping from a kube.FailureKind to its
// presentation on each path: the search chip reason, the list/detail state
// selection, and the stream handshake HTTP status. Every path classifies the
// upstream error once (kube.ClassifyError) and reads its presentation here, so
// the same failure looks consistent across list, detail, search, and stream.
// The mapping is total because the kind taxonomy is total.

// failureChipReason maps a failure kind to the short label on the search
// `.ro-scope-chip.err` chip. Only timeout, unreachable, and forbidden carry a
// distinct label; every other kind reads as the generic "failed" (the full
// per-error detail rides in the banner summary).
func failureChipReason(kind kube.FailureKind) string {
	switch kind {
	case kube.FailureTimeout:
		return "timeout"
	case kube.FailureUnreachable:
		return "unreachable"
	case kube.FailureForbidden:
		return "forbidden"
	default:
		return "failed"
	}
}

// failureListState selects the whole-list/detail failure state for a failure:
// stateForbidden for a forbidden/unauthorized denial, stateUnreachable for a
// transport failure (timeout/unreachable), an apiserver 5xx, or an unrecognized
// transport-level error, and (false) no state card for a not-found or any other
// apiserver status (a 400/409/429 the apiserver answered) -- those keep their
// existing handling. The bool reports whether a state card applies.
//
// The internal kind is the one bucket whose state depends on more than the kind:
// an apiserver-answered 400/429 (internal AND a typed Status) gets no card, while
// an unrecognized transport-level error (internal but NOT a Status) reads as
// unreachable. That split mirrors the original apiStatus tie-break, so the error
// is consulted only to resolve it.
func failureListState(kind kube.FailureKind, err error) (listStateKind, bool) {
	switch kind {
	case kube.FailureForbidden, kube.FailureUnauthorized:
		return stateForbidden, true
	case kube.FailureTimeout, kube.FailureUnreachable, kube.FailureUpstream5xx:
		return stateUnreachable, true
	case kube.FailureInternal:
		if kube.IsAPIStatusError(err) {
			return 0, false
		}
		return stateUnreachable, true
	default:
		// FailureNotFound and any future kind without a state card.
		return 0, false
	}
}

// failureHandshakeStatus maps a failure kind to the initial-list stream
// handshake HTTP status: 403 for a forbidden/unauthorized denial, 404 for a
// missing resource, and 502 for everything else (the cluster could not serve the
// snapshot).
func failureHandshakeStatus(kind kube.FailureKind) int {
	switch kind {
	case kube.FailureForbidden, kube.FailureUnauthorized:
		return http.StatusForbidden
	case kube.FailureNotFound:
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}
