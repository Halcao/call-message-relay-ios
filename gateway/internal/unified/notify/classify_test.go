package notify

import "testing"

func TestClassifyCarrierSMSArrears(t *testing.T) {
	allow := []string{"10086", "10010", "10000"}
	for _, body := range []string{
		"您的话费余额不足，请及时充值。",
		"尊敬的用户，您的号码已欠费，即将停机，请尽快充值。",
		"您的号码已停机，请缴清欠费后复机。",
		"您的账户余额不足，为避免影响使用请及时充值。",
	} {
		got := ClassifyCarrierSMS("10086", body, allow)
		if got.Verdict != VerdictArrears {
			t.Errorf("want arrears for %q, got %s (%s)", body, got.Verdict, got.Reason)
		}
	}
}

func TestClassifyCarrierSMSRestoredWins(t *testing.T) {
	allow := []string{"10086"}
	for _, body := range []string{
		"您已成功充值100元，已复机，感谢使用。",
		"您的欠费已缴清，服务已恢复。",
		"充值成功，余额已恢复。",
	} {
		got := ClassifyCarrierSMS("10086", body, allow)
		if got.Verdict != VerdictRestored {
			t.Errorf("want restored for %q, got %s (%s)", body, got.Verdict, got.Reason)
		}
	}
}

func TestClassifyCarrierSMSConditionalIsNotConfirmed(t *testing.T) {
	allow := []string{"10086"}
	for _, body := range []string{
		"您本月账单已出，请于10日前缴费，逾期将停机。",
		"如未及时充值，您的号码将暂停服务。",
		"若您欠费将影响正常使用。",
	} {
		got := ClassifyCarrierSMS("10086", body, allow)
		if got.Verdict == VerdictArrears {
			t.Errorf("conditional warning must not be confirmed arrears: %q -> %s", body, got.Reason)
		}
	}
	if got := ClassifyCarrierSMS("10086", "您本月账单已出，请于10日前缴费，逾期将停机。", allow); got.Verdict == VerdictArrears {
		t.Fatalf("bill reminder must not be confirmed arrears, got %s", got.Verdict)
	}
	if got := ClassifyCarrierSMS("10086", "若您欠费将影响正常使用。", allow); got.Verdict != VerdictConditional {
		t.Fatalf("conditional arrears wording must downgrade, got %s (%s)", got.Verdict, got.Reason)
	}
}

func TestClassifyCarrierSMSMarketingIgnored(t *testing.T) {
	allow := []string{"10086"}
	got := ClassifyCarrierSMS("10086", "尊敬的客户，5G套餐限时优惠，点击办理，回复TD退订。", allow)
	if got.Verdict != VerdictMarketing {
		t.Fatalf("want marketing, got %s", got.Verdict)
	}
}

func TestClassifyCarrierSMSSenderAllowlist(t *testing.T) {
	allow := []string{"10086"}
	got := ClassifyCarrierSMS("10690001", "您的号码已欠费停机。", allow)
	if got.Verdict != VerdictOther || got.Reason != "sender_not_allowlisted" {
		t.Fatalf("non-allowlisted sender must be ignored, got %s (%s)", got.Verdict, got.Reason)
	}
	if got := ClassifyCarrierSMS("+86 10086", "您的号码已欠费停机。", allow); got.Verdict != VerdictArrears {
		t.Fatalf("country-prefixed sender should match, got %s", got.Verdict)
	}
}

func TestClassifyCompletedBeatsConditional(t *testing.T) {
	got := ClassifyCarrierSMS("10086", "您的号码已欠费，即将停机，请尽快充值。", []string{"10086"})
	if got.Verdict != VerdictArrears {
		t.Fatalf("an already-stated arrears must stay confirmed, got %s (%s)", got.Verdict, got.Reason)
	}
}

func TestTailAndLanguage(t *testing.T) {
	if got := Tail("+86 155 5016 4926"); got != "4926" {
		t.Fatalf("tail wrong: %q", got)
	}
	if got := Tail("123"); got != "123" {
		t.Fatalf("short tail wrong: %q", got)
	}
	zh, _ := AlertContent("network_unavailable", "zh-CN", "线路2", "4926")
	if zh != "备用网络不可用" {
		t.Fatalf("zh title wrong: %q", zh)
	}
	_, body := AlertContent("arrears", "zh-CN", "线路2", "4926")
	if want := "线路2（尾号4926）"; !contains(body, want) {
		t.Fatalf("arrears body must name line and tail, got %q", body)
	}
	enTitle, _ := AlertContent("network_unavailable", "en-US", "Line 2", "4926")
	if enTitle != "Backup network unavailable" {
		t.Fatalf("en title wrong: %q", enTitle)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
