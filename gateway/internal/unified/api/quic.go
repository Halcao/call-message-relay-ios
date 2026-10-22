package api

import (
	"encoding/json"
	"net/http"

	"callrelay/gateway/internal/unified/store"
)

// mediaQUICInfo serves the authenticated QUIC capability + pin document.
// It answers enabled:false when the listener is absent or not live, so old
// clients and disabled gateways behave exactly as before (build 61 is
// purely additive).
func (s *Server) mediaQUICInfo(w http.ResponseWriter, _ *http.Request, _ store.Access) {
	if s.QUIC == nil || !s.QUIC.Live() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.QUIC.Info())
}

// mediaQUICWarm issues (or reuses) a single-use warm-probe grant for the
// authenticated device and coordinates the bounded device-grant punch.
// The grant authorizes one pre-call QUIC probe that performs no call
// action (NAT warm-up + genuine reachability/RTT evidence).
func (s *Server) mediaQUICWarm(w http.ResponseWriter, r *http.Request, access store.Access) {
	if s.QUIC == nil || !s.QUIC.Live() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	var req struct {
		Candidate string `json:"candidate"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	res, err := s.QUIC.IssueWarmGrant(access.Device.ID, req.Candidate)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, "CB-V2-429", "请求过于频繁")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type quicCandidateRequest struct {
	Candidate    string `json:"candidate"`
	NetworkEpoch string `json:"networkEpoch"`
}

// callQUICCandidate is the per-call candidate exchange: it enforces the
// same call guards as the WS media upgrade, validates the posted candidate
// (literal public IP:port; no loopback/multicast/reserved/private unless
// LANPunch is explicitly enabled), enforces the punch rate bounds and
// returns the single-use punch token plus the gateway's current candidate.
func (s *Server) callQUICCandidate(w http.ResponseWriter, r *http.Request, access store.Access) {
	if s.QUIC == nil || !s.QUIC.Live() {
		writeError(w, http.StatusServiceUnavailable, "CB-V2-503", "QUIC 直连当前不可用")
		return
	}
	callID := r.PathValue("id")
	if err := s.Manager.CheckWSMediaAccess(r.Context(), access.Device.ID, callID); err != nil {
		writeFailure(w, err)
		return
	}
	var req quicCandidateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "请求格式错误")
		return
	}
	res, err := s.QUIC.StartPunch(access.Device.ID, callID, req.Candidate)
	if err != nil {
		// Validation/rate-limit failures are client-actionable; keep the
		// message truthful but never echo internals.
		writeError(w, http.StatusBadRequest, "CB-V2-400", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
