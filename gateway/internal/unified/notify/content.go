package notify

import (
	"fmt"
	"strings"
)

// AlertContent builds the concise product notification text for one alert
// type and locale. The text names the line and the card tail only: never the
// full number, never an SMS body, no engineering jargon.
func AlertContent(alert, locale, lineName, tail string) (string, string) {
	label := strings.TrimSpace(lineName)
	if label == "" {
		label = "线路"
	}
	suffix := ""
	if tail != "" {
		suffix = "（尾号" + tail + "）"
	}
	english := strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en")
	if english {
		return alertContentEN(alert, label, tail)
	}
	switch alert {
	case "arrears":
		return label + " 欠费提醒",
			fmt.Sprintf("%s%s 已欠费或余额不足，请及时充值，避免影响使用。", label, suffix)
	case "network_unavailable":
		return "备用网络不可用",
			fmt.Sprintf("指定线路%s 已连续无法联网，可能因欠费或停机；通话不受影响。", suffix)
	case "network_recovered":
		return "备用网络已恢复",
			fmt.Sprintf("%s%s 的网络已恢复正常。", label, suffix)
	case "restored":
		return label + " 已恢复",
			fmt.Sprintf("%s%s 的服务已恢复正常。", label, suffix)
	default:
		return "CallRelay 服务提醒", "线路状态发生变化，请打开 App 查看。"
	}
}

func alertContentEN(alert, label, tail string) (string, string) {
	suffix := ""
	if tail != "" {
		suffix = " (ending " + tail + ")"
	}
	switch alert {
	case "arrears":
		return label + " arrears notice",
			fmt.Sprintf("The designated line%s may be out of credit. Please top up to keep it active.", suffix)
	case "network_unavailable":
		return "Backup network unavailable",
			fmt.Sprintf("The designated line%s cannot reach the internet; it may be suspended or out of credit. Calls are unaffected.", suffix)
	case "network_recovered":
		return "Backup network restored",
			fmt.Sprintf("The designated line%s is back online.", suffix)
	case "restored":
		return label + " restored",
			fmt.Sprintf("Service on the designated line%s is back to normal.", suffix)
	default:
		return "CallRelay service alert", "Open the app for the latest line status."
	}
}

// LanguageTag maps a BCP-47 locale to the payload language hint.
func LanguageTag(locale string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en") {
		return "en-US"
	}
	return "zh-CN"
}

// Tail returns the last four digits of a phone number for identification.
func Tail(number string) string {
	digits := digitsOnly(number)
	if len(digits) <= 4 {
		return digits
	}
	return digits[len(digits)-4:]
}
