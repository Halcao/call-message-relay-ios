package runtime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"callrelay/gateway/internal/unified/media"
)

// recoveryBeforeExpire is a test seam: when set it runs after the deadline
// wins its claim but before onExpire mutates call state, letting a test
// inject a concurrent fresh-media resolution deterministically.
var recoveryBeforeExpire func(key string)

// recoveryDeadlineFired is the timer body. The claim against a concurrent
// restore is serialized by ep.endMu and is fully decided BEFORE any terminal
// mutation, so a recovered call can never be ended by a (real or stale)
// deadline callback.
func (m *Manager) recoveryDeadlineFired(key string, ep *recoveryEpisode, onExpire func()) {
	// Serialize the claim against a concurrent fresh-media resolution for
	// the SAME episode before touching m.graces or call state.
	ep.endMu.Lock()
	defer ep.endMu.Unlock()
	if ep.resolved {
		// Fresh-media resolution won the claim first.
		return
	}
	m.mu.Lock()
	current := m.graces[key]
	same := current == ep
	if same {
		delete(m.graces, key)
	}
	m.mu.Unlock()
	if !same {
		// Superseded/cancelled: never end a newer episode's call.
		return
	}
	ep.resolved = true
	if recoveryBeforeExpire != nil {
		recoveryBeforeExpire(key)
	}
	onExpire()
}

// recoveryEpisode is one bounded host-loss recovery window (build 48).
//
// Semantics the plan requires:
//   - ONE ABSOLUTE deadline: firstLoss + the resolved 45 s grace. Repeated
//     loss callbacks while an episode is live neither restart nor extend it.
//   - Generation fence: the deadline callback captures gen; a newer episode
//     (recovery that already succeeded and failed again) bumps gen, and the
//     Close path cancels explicitly — a stale timer can therefore never end
//     a recovered call (same pattern as preflightEntry; time.Timer.Reset is
//     intentionally NOT used).
//   - Recovery clears ONLY on fresh-media proof (media.Session fires
//     onHostRestored on the first genuine inbound frame after reattach), not
//     on the attach/CONNECTED control event and not on an old RTT sample.
//   - Explicit local/remote/system end and revoked auth bypass the window.
//
// The daemon itself does not redial: the client drives immediate +
// 1/2/4/8 s reconnect attempts of the SAME call against this window, which
// is why calls and conferences share one duration and one deadline.
type recoveryEpisode struct {
	key      string
	deadline time.Time
	gen      uint64
	timer    *time.Timer
	// endMu serializes the terminal deadline transition against a
	// fresh-media resolution. The winning claim (whichever acquires first)
	// performs the whole transition under the mutex, so a restore injected
	// while the deadline callback is already ending cannot half-restore a
	// call the callback is concurrently hanging up, and a callback that lost
	// its claim aborts BEFORE touching call state.
	endMu    sync.Mutex
	resolved bool
}

// beginRecovery opens the loss episode for a call/conference unless one is
// already live (repeated errors must not extend the deadline). It returns
// true when THIS call opened a new episode, so callers only persist/publish
// state once. onExpire runs only when the absolute deadline fires for the
// still-current generation.
// graceOverride is a test-only seam short-circuiting the resolved grace
// duration (zero = use DisconnectGrace). Never set outside tests.
var testGraceOverride time.Duration

func (m *Manager) recoveryGrace() time.Duration {
	if testGraceOverride != 0 {
		return testGraceOverride
	}
	return m.DisconnectGrace(context.Background())
}

func (m *Manager) beginRecovery(key string, onExpire func()) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	if ep := m.graces[key]; ep != nil {
		m.mu.Unlock()
		return false
	}
	deadline := time.Now().UTC().Add(m.recoveryGrace())
	ep := &recoveryEpisode{key: key, deadline: deadline, gen: 1}
	gen := ep.gen
	timer := time.AfterFunc(time.Until(deadline), func() {
		m.recoveryDeadlineFired(key, ep, onExpire)
	})
	ep.timer = timer
	m.graces[key] = ep
	m.mu.Unlock()
	_ = gen
	return true
}

// resolveRecovery claims the live episode for a fresh-media resolution. The
// claim is serialized against the deadline callback by ep.endMu and is fully
// won BEFORE the deadline body can mutate call state: a restore injected
// while the deadline is already firing either happens-before it (it wins and
// the deadline returns at ep.resolved) or after it (it no-ops, the call was
// genuinely not recovered within the window). Never a half-restore of a call
// that is simultaneously being hung up. Safe when no episode exists.
func (m *Manager) resolveRecovery(key string) bool {
	m.mu.Lock()
	ep := m.graces[key]
	delete(m.graces, key)
	m.mu.Unlock()
	if ep == nil {
		return false
	}
	ep.endMu.Lock()
	already := ep.resolved
	if !already {
		ep.resolved = true
		ep.timer.Stop()
	}
	ep.endMu.Unlock()
	return !already
}

// cancelRecovery is retained as the explicit-end entry point name.
func (m *Manager) cancelRecovery(key string) { m.resolveRecovery(key) }

// recoveryDeadline reports the absolute deadline of the live episode (if
// any) for persistence and evidence.
func (m *Manager) recoveryDeadline(key string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep := m.graces[key]; ep != nil {
		return ep.deadline, true
	}
	return time.Time{}, false
}

// onHostLost is the media callback for a host transport going away.
func (m *Manager) onHostLost(key string, cause media.HostLossCause) {
	if cause == media.HostLossAuthRevoked {
		// Revoked authorization is terminal: never enter a recovery window.
		m.terminateForRevocation(key)
		return
	}
	call, callErr := m.Store.GetCall(context.Background(), key)
	conference, conferenceErr := m.Store.GetConference(context.Background(), key)
	switch {
	case callErr == nil && call.EndedAt == nil:
		m.publish(call.LineID, "", "call.updated", redactedCallView(m.view(call)))
		if m.beginRecovery(key, func() {
			current, err := m.Store.GetCall(context.Background(), key)
			if err != nil || current.EndedAt != nil {
				return
			}
			slog.Info("call media recovery deadline expired; hanging up", "call_id", key)
			_ = m.hangupCall(context.Background(), current, "media_timeout")
		}) {
			slog.Info("call host lost; recovery window opened", "call_id", key,
				"grace_seconds", int(m.DisconnectGrace(context.Background()).Seconds()))
		}
	case conferenceErr == nil && conference.EndedAt == nil:
		opened := m.beginRecovery(key, func() {
			slog.Info("conference recovery deadline expired; ending all legs", "conference_id", key)
			_ = m.CloseConferenceAsSystem(context.Background(), key, "host_timeout")
		})
		if opened {
			deadline, _ := m.recoveryDeadline(key)
			conference.State = "host_disconnected"
			conference.GraceDeadline = &deadline
			_ = m.Store.UpdateConference(context.Background(), conference)
			m.publish("", "", "conference.updated", m.conferenceView(conference))
			slog.Info("conference host lost; recovery window opened", "conference_id", key,
				"grace_seconds", int(m.DisconnectGrace(context.Background()).Seconds()))
		}
	}
}

// onHostRestored fires only on FRESH-MEDIA proof from the media layer (the
// first genuine inbound frame after a reattach), never on attach alone.
func (m *Manager) onHostRestored(key string) {
	if !m.resolveRecovery(key) {
		return
	}
	slog.Info("host fresh media proved; recovery window closed", "key", key)
	if conference, err := m.Store.GetConference(context.Background(), key); err == nil && conference.EndedAt == nil {
		conference.State = "active"
		conference.GraceDeadline = nil
		_ = m.Store.UpdateConference(context.Background(), conference)
		m.publish("", "", "conference.updated", m.conferenceView(conference))
	}
	if call, err := m.Store.GetCall(context.Background(), key); err == nil && call.EndedAt == nil {
		m.publish(call.LineID, "", "call.updated", redactedCallView(m.view(call)))
	}
}

// terminateForRevocation ends a live call/conference immediately when the
// host transport closed because the device authorization was revoked.
func (m *Manager) terminateForRevocation(key string) {
	m.resolveRecovery(key)
	if call, err := m.Store.GetCall(context.Background(), key); err == nil && call.EndedAt == nil {
		slog.Info("host media closed for revoked authorization; ending call", "call_id", key)
		_ = m.hangupCall(context.Background(), call, "auth_revoked")
		return
	}
	if conference, err := m.Store.GetConference(context.Background(), key); err == nil && conference.EndedAt == nil {
		slog.Info("conference media closed for revoked authorization; ending all legs", "conference_id", key)
		_ = m.CloseConferenceAsSystem(context.Background(), key, "auth_revoked")
	}
}
