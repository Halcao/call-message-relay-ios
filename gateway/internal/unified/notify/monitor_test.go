package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/store"
)

type fakePush struct {
	mu       sync.Mutex
	alerts   []push.AlertPayload
	devices  []string
	attempts map[string]int
	failures map[string]int
}

func (f *fakePush) SendVoIP(context.Context, push.Target, push.VoIPPayload) error { return nil }
func (f *fakePush) SendMessage(context.Context, push.Target, push.MessagePayload) error {
	return nil
}
func (f *fakePush) SendAlert(_ context.Context, target push.Target, payload push.AlertPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	if f.failures == nil {
		f.failures = map[string]int{}
	}
	f.attempts[target.DeviceID]++
	if f.failures[target.DeviceID] > 0 {
		f.failures[target.DeviceID]--
		return errors.New("simulated push failure")
	}
	f.alerts = append(f.alerts, payload)
	f.devices = append(f.devices, target.DeviceID)
	return nil
}

func (f *fakePush) failTimes(device string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = map[string]int{}
	}
	f.failures[device] += times
}

func (f *fakePush) count(alert string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, item := range f.alerts {
		if alert == "" || item.Alert == alert {
			n++
		}
	}
	return n
}

func (f *fakePush) deliveredFor(device, alert string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i, item := range f.alerts {
		if f.devices[i] == device && (alert == "" || item.Alert == alert) {
			n++
		}
	}
	return n
}

func (f *fakePush) attemptsFor(device string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[device]
}

func (f *fakePush) snapshot() []push.AlertPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]push.AlertPayload(nil), f.alerts...)
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "notify.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := database.EnsureLine(ctx, store.Line{ID: "line2", Name: "线路2", WorkerURL: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatalf("ensure line: %v", err)
	}
	if err := database.EnsureLine(ctx, store.Line{ID: "line3", Name: "线路3", WorkerURL: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatalf("ensure line3: %v", err)
	}
	if err := database.CreateEnrollmentKey(ctx, "key_a", "用户A", []byte{1}, "secret-a", true); err != nil {
		t.Fatalf("create key a: %v", err)
	}
	if err := database.CreateEnrollmentKey(ctx, "key_b", "用户B", []byte{2}, "secret-b", false); err != nil {
		t.Fatalf("create key b: %v", err)
	}
	if err := database.CreateDevice(ctx, store.Device{ID: "dev_a", KeyID: "key_a", Name: "phone-a", PublicKey: []byte{1}}); err != nil {
		t.Fatalf("create device a: %v", err)
	}
	if err := database.SetDevicePush(ctx, "dev_a", store.PushRegistration{
		APNSToken: "aabbcc", Environment: "sandbox", Locale: "zh-CN",
	}); err != nil {
		t.Fatalf("set push a: %v", err)
	}
	if err := database.CreateDevice(ctx, store.Device{ID: "dev_b", KeyID: "key_b", Name: "phone-b", PublicKey: []byte{2}}); err != nil {
		t.Fatalf("create device b: %v", err)
	}
	if err := database.SetDevicePush(ctx, "dev_b", store.PushRegistration{
		APNSToken: "ddeeff", Environment: "sandbox", Locale: "zh-CN",
	}); err != nil {
		t.Fatalf("set push b: %v", err)
	}
	if err := database.SaveLineIdentity(ctx, store.LineIdentity{LineID: "line2", PhoneNumber: "15550164926"}); err != nil {
		t.Fatalf("line identity: %v", err)
	}
	if err := database.SaveLineIdentity(ctx, store.LineIdentity{LineID: "line3", PhoneNumber: "13900139000"}); err != nil {
		t.Fatalf("line3 identity: %v", err)
	}
	return database
}

// addDevice enrolls another device under an existing key with a standard
// APNs token.
func addDevice(t *testing.T, database *store.Store, deviceID, keyID, publicKey, token string) {
	t.Helper()
	ctx := context.Background()
	if err := database.CreateDevice(ctx, store.Device{ID: deviceID, KeyID: keyID, Name: deviceID, PublicKey: []byte(publicKey)}); err != nil {
		t.Fatalf("create device %s: %v", deviceID, err)
	}
	if err := database.SetDevicePush(ctx, deviceID, store.PushRegistration{
		APNSToken: token, Environment: "sandbox", Locale: "zh-CN",
	}); err != nil {
		t.Fatalf("set push %s: %v", deviceID, err)
	}
}

func newTestMonitor(t *testing.T, database *store.Store, sender push.Sender, results ...ProbeResult) *Monitor {
	t.Helper()
	index := 0
	monitor := New(database, sender, Config{
		Enabled: true, GatewayID: "callrelay-test", WanLineID: "line2",
		WanStateFile:      filepath.Join(t.TempDir(), "missing-state"),
		SustainedFailures: 2, RecoverySuccesses: 2,
	})
	monitor.RunProbe = func(context.Context) (ProbeResult, string) {
		if index < len(results) {
			result := results[index]
			index++
			return result, ""
		}
		if len(results) > 0 {
			return results[len(results)-1], ""
		}
		return ProbeUnknown, ""
	}
	return monitor
}

func incidentFor(status Status, lineID string) *Incident {
	for i := range status.Incidents {
		if status.Incidents[i].LineID == lineID {
			return &status.Incidents[i]
		}
	}
	return nil
}

func TestNetworkFaultIsOneAlertAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail)
	if err := monitor.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count(""); got != 0 {
		t.Fatalf("one failed probe is not sustained yet, got %d alerts", got)
	}
	if err := monitor.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count("network_unavailable"); got != 1 {
		t.Fatalf("want exactly one network alert, got %d", got)
	}
	if err := monitor.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count(""); got != 1 {
		t.Fatalf("repeated fault must stay one alert, got %d", got)
	}
	// A restart (fresh monitor over the same store) must not re-alert.
	restarted := newTestMonitor(t, database, sender, ProbeFail, ProbeFail)
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count(""); got != 1 {
		t.Fatalf("restart must not replay the fault alert, got %d", got)
	}
	if restarted.Status(ctx).Availability != AvailabilityUnavailable {
		t.Fatalf("availability should persist: %s", restarted.Status(ctx).Availability)
	}
}

func TestNetworkRecoveryAlertsOnceAndNamesTail(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail, ProbeOK, ProbeOK, ProbeOK)
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_recovered"); got != 0 {
		t.Fatalf("one successful probe is not sustained recovery, got %d", got)
	}
	if err := monitor.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count("network_recovered"); got != 1 {
		t.Fatalf("want one recovery alert, got %d", got)
	}
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_recovered"); got != 1 {
		t.Fatalf("recovery repeated: %d", got)
	}
	snapshot := sender.snapshot()
	if len(snapshot) == 0 {
		t.Fatal("no alerts recorded")
	}
	if snapshot[0].LineName != "线路2" || snapshot[0].Body == "" {
		t.Fatalf("alert must name the line and carry body: %+v", snapshot[0])
	}
	if !countSubstring(snapshot[0].Body, "4926") {
		t.Fatalf("alert body should identify the card tail: %q", snapshot[0].Body)
	}
}

func TestArrearsSMSDedupeAndGenericSuppression(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail, ProbeOK, ProbeOK)
	monitor.ObserveInboundSMS(ctx, "line2", "msg_1", "10086", "您的号码已欠费停机，请及时缴费。")
	monitor.ObserveInboundSMS(ctx, "line2", "msg_1", "10086", "您的号码已欠费停机，请及时缴费。")
	if got := sender.count("arrears"); got != 1 {
		t.Fatalf("want one arrears alert, got %d", got)
	}
	// The generic network alert must not follow once the cause is known.
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_unavailable"); got != 0 {
		t.Fatalf("generic alert must be suppressed while arrears is known, got %d", got)
	}
	if got := sender.count(""); got != 1 {
		t.Fatalf("total alerts after suppressed fault: %d", got)
	}
	// Recovery is still announced exactly once. The incident cause is
	// arrears, so the recovery uses the "restored" wording.
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_recovered") + sender.count("restored"); got != 1 {
		t.Fatalf("want one recovery alert, got %d", got)
	}
}

func TestRepeatedArrearsMessagesOneIncident(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeUnknown)
	monitor.ObserveInboundSMS(ctx, "line2", "msg_a", "10086", "您的号码已欠费，请及时缴费。")
	monitor.ObserveInboundSMS(ctx, "line2", "msg_b", "10086", "余额不足，为不影响使用请充值。")
	monitor.ObserveInboundSMS(ctx, "line2", "msg_c", "10086", "您已欠费，请尽快处理。")
	if got := sender.count("arrears"); got != 1 {
		t.Fatalf("one open incident must produce one arrears alert, got %d", got)
	}
	// After a restore, a new arrears message is a NEW incident and alerts.
	monitor.ObserveInboundSMS(ctx, "line2", "msg_d", "10086", "充值成功，已复机。")
	if got := sender.count("restored") + sender.count("network_recovered"); got != 1 {
		t.Fatalf("restore alert count: %d", got)
	}
	monitor.ObserveInboundSMS(ctx, "line2", "msg_e", "10086", "您的号码已欠费停机。")
	if got := sender.count("arrears"); got != 2 {
		t.Fatalf("a new incident must alert again, got %d", got)
	}
}

func TestIncidentsArePerLineAndRecoveryIsLineScoped(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail, ProbeOK, ProbeOK)
	// Arrears on a non-designated line.
	monitor.ObserveInboundSMS(ctx, "line3", "msg_x", "10086", "您的号码已欠费停机。")
	if got := sender.count("arrears"); got != 1 {
		t.Fatalf("line3 arrears: %d", got)
	}
	// Network fault on the designated line.
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_unavailable"); got != 1 {
		t.Fatalf("line2 network alert: %d", got)
	}
	status := monitor.Status(ctx)
	if incidentFor(status, "line3") == nil || incidentFor(status, "line2") == nil {
		t.Fatalf("both lines need independent incidents: %+v", status.Incidents)
	}
	// The designated line recovers; line3's arrears incident must survive.
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	status = monitor.Status(ctx)
	if incidentFor(status, "line2") != nil {
		t.Fatalf("line2 incident should be closed: %+v", status.Incidents)
	}
	if incidentFor(status, "line3") == nil {
		t.Fatalf("line3 incident was wrongly closed by line2 recovery: %+v", status.Incidents)
	}
	if got := sender.count("network_recovered"); got != 1 {
		t.Fatalf("line2 recovery count: %d", got)
	}
	if got := sender.count("network_recovered") + sender.count("restored"); got != 1 {
		t.Fatalf("line3 must not receive a recovery yet: %d", got)
	}
	// Only the line3 restore message closes its incident.
	monitor.ObserveInboundSMS(ctx, "line3", "msg_y", "10086", "您已缴费成功，服务已恢复。")
	if got := sender.count("restored"); got != 1 {
		t.Fatalf("line3 restore alert: %d", got)
	}
	if incidentFor(monitor.Status(ctx), "line3") != nil {
		t.Fatalf("line3 incident should be closed after restore")
	}
}

func TestGenericThenSMSUpdatesWithoutSpam(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail, ProbeOK, ProbeOK)
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_unavailable"); got != 1 {
		t.Fatalf("want generic alert, got %d", got)
	}
	monitor.ObserveInboundSMS(ctx, "line2", "msg_2", "10086", "您的号码已欠费停机。")
	if got := sender.count(""); got != 1 {
		t.Fatalf("SMS after generic must update state, not send a second alert: %d", got)
	}
	incident := incidentFor(monitor.Status(ctx), "line2")
	if incident == nil || incident.Cause != "arrears" || !incident.ArrearsNotified {
		t.Fatalf("incident should record the SMS cause: %+v", incident)
	}
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	if got := sender.count("network_recovered") + sender.count("restored"); got != 1 {
		t.Fatalf("want one recovery alert, got %d", got)
	}
	if got := sender.count(""); got != 2 {
		t.Fatalf("final alert count: %d", got)
	}
}

func TestRestoredSMSRecoveryOnce(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeUnknown)
	monitor.ObserveInboundSMS(ctx, "line2", "msg_3", "10086", "您的号码已欠费停机。")
	monitor.ObserveInboundSMS(ctx, "line2", "msg_4", "10086", "您已充值成功，已复机。")
	monitor.ObserveInboundSMS(ctx, "line2", "msg_5", "10086", "余额已恢复，感谢使用。")
	if got := sender.count("arrears"); got != 1 {
		t.Fatalf("arrears count: %d", got)
	}
	if got := sender.count("network_recovered") + sender.count("restored"); got != 1 {
		t.Fatalf("recovery must be once, got %d (alerts %+v)", got, sender.snapshot())
	}
}

func TestLineACLAndNotifyMode(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail)
	_ = monitor.RunOnce(ctx)
	_ = monitor.RunOnce(ctx)
	for _, device := range sender.devices {
		if device == "dev_b" {
			t.Fatalf("device without line2 access must not receive the alert")
		}
	}
	if got := sender.count("network_unavailable"); got != 1 {
		t.Fatalf("authorized device should receive exactly one alert, got %d", got)
	}
	// Recover the first incident.
	restarted := newTestMonitor(t, database, sender, ProbeOK, ProbeOK)
	_ = restarted.RunOnce(ctx)
	_ = restarted.RunOnce(ctx)
	if got := sender.count("network_recovered"); got != 1 {
		t.Fatalf("recovery count: %d", got)
	}
	if err := database.SetDeviceNotifyMode(ctx, "dev_a", store.NotifyModeWeb); err != nil {
		t.Fatalf("set notify mode: %v", err)
	}
	// A new incident after the preference change is not sent natively.
	webOnly := newTestMonitor(t, database, sender, ProbeFail, ProbeFail)
	_ = webOnly.RunOnce(ctx)
	_ = webOnly.RunOnce(ctx)
	if got := sender.count(""); got != 2 {
		t.Fatalf("web-only device must not receive a native alert, alerts=%d", got)
	}
}

func TestPerTargetDeliveryIsRetriedWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	addDevice(t, database, "dev_a2", "key_a", "9", "998877")
	sender := &fakePush{}
	sender.failTimes("dev_a2", 2)
	monitor := newTestMonitor(t, database, sender, ProbeFail, ProbeFail, ProbeUnknown, ProbeUnknown, ProbeUnknown)
	_ = monitor.RunOnce(ctx) // not sustained yet
	_ = monitor.RunOnce(ctx) // fault: dev_a succeeds, dev_a2 fails (1/2)
	if got := sender.deliveredFor("dev_a", "network_unavailable"); got != 1 {
		t.Fatalf("first device should have received exactly one alert, got %d", got)
	}
	if got := sender.deliveredFor("dev_a2", "network_unavailable"); got != 0 {
		t.Fatalf("failed target should not be recorded delivered: %d", got)
	}
	_ = monitor.RunOnce(ctx) // retry: dev_a2 fails again (2/2)
	if got := sender.deliveredFor("dev_a2", "network_unavailable"); got != 0 {
		t.Fatalf("second failed attempt must still be pending: %d", got)
	}
	_ = monitor.RunOnce(ctx) // retry: dev_a2 succeeds
	if got := sender.deliveredFor("dev_a2", "network_unavailable"); got != 1 {
		t.Fatalf("retry should deliver to the failed device, got %d", got)
	}
	if got := sender.deliveredFor("dev_a", "network_unavailable"); got != 1 {
		t.Fatalf("successful device was re-notified: %d", got)
	}
	pending, err := database.PendingAlertEvents(ctx, 5, 20)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("alert should be retired once every target accepted: %+v", pending)
	}
}

func TestSlowProbeAndSMSNeverBlockStatus(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := New(database, sender, Config{
		Enabled: true, GatewayID: "callrelay-test", WanLineID: "line2",
		WanStateFile:      filepath.Join(t.TempDir(), "missing-state"),
		SustainedFailures: 2, RecoverySuccesses: 2,
	})
	release := make(chan struct{})
	monitor.RunProbe = func(probeCtx context.Context) (ProbeResult, string) {
		select {
		case <-release:
		case <-probeCtx.Done():
		}
		return ProbeUnknown, ""
	}
	done := make(chan error, 1)
	go func() { done <- monitor.RunOnce(ctx) }()

	// While the probe is blocked, Status and inbound-SMS handling must return
	// promptly (the state lock is not held during network I/O).
	statusDone := make(chan Status, 1)
	go func() { statusDone <- monitor.Status(ctx) }()
	select {
	case status := <-statusDone:
		if status.LineID != "line2" {
			t.Fatalf("unexpected status: %+v", status)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Status blocked on a slow probe")
	}
	smsDone := make(chan struct{})
	go func() {
		monitor.ObserveInboundSMS(ctx, "line2", "msg_slow", "10086", "您的号码已欠费停机。")
		close(smsDone)
	}()
	select {
	case <-smsDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ObserveInboundSMS blocked on a slow probe")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnce did not finish after the probe returned")
	}
}

func TestNilMonitorStatusIsDisabled(t *testing.T) {
	var monitor *Monitor
	status := monitor.Status(context.Background())
	if status.Enabled || status.Availability != AvailabilityUnknown {
		t.Fatalf("nil monitor status: %+v", status)
	}
}

func TestAgentDegradedStateRaisesAlert(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	statePath := filepath.Join(t.TempDir(), "state")
	if err := writeFile(statePath, "state=degraded\ndetail=fallback link lost\nupdated=2026-10-05T00:00:00Z\n"); err != nil {
		t.Fatalf("write state: %v", err)
	}
	monitor := New(database, sender, Config{
		Enabled: true, GatewayID: "callrelay-test", WanLineID: "line2",
		WanStateFile: statePath, SustainedFailures: 2, RecoverySuccesses: 2,
	})
	monitor.RunProbe = func(context.Context) (ProbeResult, string) { return ProbeUnknown, "" }
	if err := monitor.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count("network_unavailable"); got != 1 {
		t.Fatalf("degraded agent state should alert once, got %d", got)
	}
	if err := monitor.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sender.count(""); got != 1 {
		t.Fatalf("degraded state must not repeat: %d", got)
	}
}

func TestProbeUnknownNeverAlerts(t *testing.T) {
	ctx := context.Background()
	database := newTestStore(t)
	sender := &fakePush{}
	monitor := newTestMonitor(t, database, sender, ProbeUnknown, ProbeUnknown, ProbeUnknown)
	for i := 0; i < 3; i++ {
		if err := monitor.RunOnce(ctx); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if got := sender.count(""); got != 0 {
		t.Fatalf("unknown probe results must not alert, got %d", got)
	}
	if status := monitor.Status(ctx); status.Availability != AvailabilityUnknown {
		t.Fatalf("availability should stay unknown: %s", status.Availability)
	}
}

func countSubstring(haystack, needle string) bool {
	return indexOf(haystack, needle) >= 0
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
