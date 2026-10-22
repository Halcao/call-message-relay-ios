// Package notify turns confirmed carrier messages and designated-backup
// network availability into ordinary Apple alert pushes. It never sends
// VoIP/PushKit pushes for non-call events, never includes an SMS body in a
// notification, and dedupes one fault to one alert durably in SQLite.
package notify

import "strings"

// Verdict is the conservative classification of one inbound SMS.
type Verdict string

const (
	// VerdictOther is anything that is not a carrier balance/arrears signal.
	VerdictOther Verdict = "other"
	// VerdictArrears is a confirmed current arrears/balance/suspension notice.
	VerdictArrears Verdict = "arrears"
	// VerdictRestored explicitly says service was restored/recharged.
	VerdictRestored Verdict = "restored"
	// VerdictMarketing is a promotional message, never an arrears alert.
	VerdictMarketing Verdict = "marketing"
	// VerdictConditional is a warning contingent on a future action
	// ("if you do not pay by X, service will stop") and is not confirmed.
	VerdictConditional Verdict = "conditional"
)

// Classification carries the verdict plus a short machine reason used in
// logs/tests. It never contains the message body.
type Classification struct {
	Verdict Verdict
	Reason  string
}

var restoredMarkers = []string{
	"充值成功", "缴费成功", "充值到账", "缴费到账", "已复机", "已恢复", "恢复正常",
	"余额已恢复", "感谢您充值", "感谢充值", "成功充值", "已为您开机", "开机成功",
	"复机成功", "已解除停机", "恢复正常使用", "服务已恢复",
}

var marketingMarkers = []string{
	"优惠", "活动", "套餐", "办理", "推荐", "点击", "退订", "抽奖", "礼品",
	"流量包", "升级", "订购", "折扣", "返费", "会员", "领用", "抢购", "限时",
	"积分", "详情请", "关注", "回复",
}

// completedMarkers are phrases that state the suspension/arrears already
// happened. They outrank conditional wording.
var completedMarkers = []string{
	"已欠费", "欠费停机", "已停机", "停止服务", "暂停服务", "服务已暂停",
	"已限制服务", "已暂停", "已停止", "欠费已",
}

var strongArrearsMarkers = []string{
	"欠费", "余额不足", "话费不足", "账户余额不足", "余额低于", "费用不足",
	"话费余额不足", "通信费",
}

var conditionalMarkers = []string{
	"如未", "如果", "若您", "否则", "即将", "将要", "逾期", "未在", "前缴费",
	"前充值", "请于", "之前完成", "到时", "将影响", "将暂停", "将停止",
}

// ClassifyCarrierSMS returns a conservative verdict for one inbound message.
// Only senders on the exact allowlist are considered; everything else is
// "other" so a spoofed or marketing short code can never raise an arrears
// alert. Restored messages take precedence over arrears wording so a
// "已缴清，已复机" notice is never reported as a new fault.
func ClassifyCarrierSMS(sender, body string, allowlist []string) Classification {
	if !senderAllowed(sender, allowlist) {
		return Classification{Verdict: VerdictOther, Reason: "sender_not_allowlisted"}
	}
	text := normalizeText(body)
	if text == "" {
		return Classification{Verdict: VerdictOther, Reason: "empty"}
	}
	for _, marker := range restoredMarkers {
		if strings.Contains(text, marker) {
			return Classification{Verdict: VerdictRestored, Reason: "restored:" + marker}
		}
	}
	hasStrong := false
	strongReason := ""
	for _, marker := range strongArrearsMarkers {
		if strings.Contains(text, marker) {
			hasStrong = true
			strongReason = marker
			break
		}
	}
	if !hasStrong {
		for _, marker := range marketingMarkers {
			if strings.Contains(text, marker) {
				return Classification{Verdict: VerdictMarketing, Reason: "marketing:" + marker}
			}
		}
		return Classification{Verdict: VerdictOther, Reason: "no_arrears_marker"}
	}
	completed := false
	for _, marker := range completedMarkers {
		if strings.Contains(text, marker) {
			completed = true
			break
		}
	}
	if !completed {
		for _, marker := range conditionalMarkers {
			if strings.Contains(text, marker) {
				return Classification{Verdict: VerdictConditional, Reason: "conditional:" + marker + ":" + strongReason}
			}
		}
	}
	return Classification{Verdict: VerdictArrears, Reason: "arrears:" + strongReason}
}

// senderAllowed matches the sender's digits exactly against the allowlist.
// A country-prefixed form (86 + number) is accepted for allowlist entries
// that are themselves full numbers; short codes stay exact.
func senderAllowed(sender string, allowlist []string) bool {
	digits := digitsOnly(sender)
	if digits == "" {
		return false
	}
	for _, entry := range allowlist {
		candidate := digitsOnly(entry)
		if candidate == "" {
			continue
		}
		if digits == candidate {
			return true
		}
		if strings.HasPrefix(digits, "86") && strings.TrimPrefix(digits, "86") == candidate {
			return true
		}
	}
	return false
}

// normalizeText lowercases and removes spaces so full-width or spaced markers
// still match; it keeps the message in memory only for classification.
func normalizeText(body string) string {
	replacer := strings.NewReplacer(
		" ", "", "\u3000", "", "\t", "", "\n", "", "\r", "",
		"Ａ", "A", "，", ",", "。", ".",
	)
	return strings.ToLower(replacer.Replace(body))
}

func digitsOnly(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}
