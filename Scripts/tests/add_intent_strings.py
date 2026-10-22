#!/usr/bin/env python3
"""Add the App-Intents Shortcuts strings to Localizable.xcstrings.

Idempotent: entries already present are skipped. The edit is purely
additive — existing keys, their order and the file formatting are left
untouched (Xcode manages this catalog).
"""
import json
from pathlib import Path

PATH = Path("CallRelay/Localizable.xcstrings")

def unit(value):
    return {"stringUnit": {"state": "translated", "value": value}}

def simple(zh, en, hant):
    return {"localizations": {
        "zh-Hans": unit(zh), "en": unit(en), "zh-Hant": unit(hant)}}

ENTRIES = {
    "用 CallRelay 拨打": simple("用 CallRelay 拨打", "Call with CallRelay", "用 CallRelay 撥打"),
    "在 CallRelay 中编写短信": simple("在 CallRelay 中编写短信", "Compose Message in CallRelay", "在 CallRelay 中編寫簡訊"),
    "打开 CallRelay": simple("打开 CallRelay", "Open CallRelay", "打開 CallRelay"),
    "通过已授权的网关线路拨打电话。可选择线路；不选择时使用现有默认线路偏好。线路不可用会在 App 内说明，绝不改用蜂窝电话。":
        simple("通过已授权的网关线路拨打电话。可选择线路；不选择时使用现有默认线路偏好。线路不可用会在 App 内说明，绝不改用蜂窝电话。",
               "Call through an authorized gateway line. Optionally pick a line; otherwise the existing default line preference applies. Unavailable lines are explained in the app — a cellular call is never placed.",
               "透過已授權的網關線路撥打電話。可選擇線路；不選擇時使用現有預設線路偏好。線路不可用會在 App 內說明，絕不改用手機電話。"),
    "在 CallRelay 中打开发短信界面并预填收件人、内容与线路；发送需在 App 内确认。":
        simple("在 CallRelay 中打开发短信界面并预填收件人、内容与线路；发送需在 App 内确认。",
               "Open the CallRelay composer prefilled with recipient, body and line; sending must be confirmed in the app.",
               "在 CallRelay 中打開發簡訊介面並預填收件人、內容與線路；傳送需在 App 內確認。"),
    "打开 CallRelay 的拨号键盘、短信或语音留言。":
        simple("打开 CallRelay 的拨号键盘、短信或语音留言。",
               "Open the CallRelay dialer, messages or voicemail.",
               "打開 CallRelay 的撥號鍵盤、簡訊或語音留言。"),
    "内容": simple("内容", "Body", "內容"),
    "目的地": simple("目的地", "Destination", "目的地"),
    "要拨打的号码，例如 +86 130 0000 0000。":
        simple("要拨打的号码，例如 +86 130 0000 0000。",
               "The number to call, e.g. +86 130 0000 0000.",
               "要撥打的號碼，例如 +86 130 0000 0000。"),
    "用于外呼的已授权线路；不选则沿用现有默认偏好。":
        simple("用于外呼的已授权线路；不选则沿用现有默认偏好。",
               "Authorized line for the call; when unset the existing default preference applies.",
               "用於外呼的已授權線路；不選則沿用現有預設偏好。"),
    "收件人号码，例如 +86 130 0000 0000。":
        simple("收件人号码，例如 +86 130 0000 0000。",
               "The recipient's number, e.g. +86 130 0000 0000.",
               "收件人號碼，例如 +86 130 0000 0000。"),
    "短信正文。": simple("短信正文。", "The message body.", "簡訊正文。"),
    "用于发送的已授权线路；不选则沿用现有默认偏好。":
        simple("用于发送的已授权线路；不选则沿用现有默认偏好。",
               "Authorized line to send from; when unset the existing default preference applies.",
               "用於傳送的已授權線路；不選則沿用現有預設偏好。"),
    "拨号键盘、短信或语音留言。":
        simple("拨号键盘、短信或语音留言。", "Dialer, messages or voicemail.", "撥號鍵盤、簡訊或語音留言。"),
    "快捷指令指定的线路已不属于当前配对（可能尚未配对、已重新配对或网关已变更）。为避免打错线路，本次不会拨打，也不会改用其他线路；请更新快捷指令或重新选择线路。":
        simple("快捷指令指定的线路已不属于当前配对（可能尚未配对、已重新配对或网关已变更）。为避免打错线路，本次不会拨打，也不会改用其他线路；请更新快捷指令或重新选择线路。",
               "The line chosen in the shortcut no longer belongs to the current pairing (not paired, re-paired, or the gateway changed). To avoid calling from the wrong line, the call was not placed and no other line was substituted; update the shortcut or pick a line again.",
               "捷徑指定的線路已不屬於目前配對（可能尚未配對、已重新配對或網關已變更）。為避免打錯線路，本次不會撥打，也不會改用其他線路；請更新捷徑或重新選擇線路。"),
    "号码不能为空。": simple("号码不能为空。", "The number can't be empty.", "號碼不能為空。"),
    "号码至少需要包含一位数字。":
        simple("号码至少需要包含一位数字。", "The number needs at least one digit.", "號碼至少需要包含一位數字。"),
    "写短信": simple("写短信", "Compose", "寫簡訊"),
    "打开": simple("打开", "Open", "打開"),
    "CallRelay 线路": simple("CallRelay 线路", "CallRelay Line", "CallRelay 線路"),
    "CallRelay 目的地": simple("CallRelay 目的地", "CallRelay Destination", "CallRelay 目的地"),
    "快捷指令指定的线路已不可用（配对已变更或线路已被取消授权）。草稿内容已保留；请在“发件人”中重新选择线路后再发送。":
        simple("快捷指令指定的线路已不可用（配对已变更或线路已被取消授权）。草稿内容已保留；请在“发件人”中重新选择线路后再发送。",
               "The line chosen in the shortcut is no longer available (pairing changed or line access revoked). Your draft is preserved; pick a line in \u201cFrom\u201d before sending.",
               "捷徑指定的線路已不可用（配對已變更或線路已被取消授權）。草稿內容已保留；請在「發件人」中重新選擇線路後再傳送。"),
}

def render(key, entry):
    lines = [f'    "{key}": {{', '      "localizations": {']
    locales = list(entry["localizations"].items())
    for i, (loc, body) in enumerate(locales):
        comma = "," if i < len(locales) - 1 else ""
        lines.append(f'        "{loc}": {{')
        lines.append('          "stringUnit": {{'.format())
        lines.append(f'            "state": "{body["stringUnit"]["state"]}",')
        lines.append(f'            "value": {json.dumps(body["stringUnit"]["value"], ensure_ascii=False)}')
        lines.append('          }')
        lines.append(f'        }}{comma}')
    lines.append('      }')
    lines.append('    }')
    return "\n".join(lines)

def main():
    raw = PATH.read_text()
    doc = json.loads(raw)
    missing = {k: v for k, v in ENTRIES.items() if k not in doc["strings"]}
    if not missing:
        print("all entries already present")
        return
    blocks = ",\n".join(render(k, v) for k, v in missing.items()) + "\n"
    marker = '  "version": "1.0"'
    idx = raw.rfind(marker)
    head, tail = raw[:idx], raw[idx:]
    assert head.endswith("    }\n  },\n"), "unexpected catalog tail shape"
    new_raw = head[:-len("    }\n  },\n")] + "    },\n" + blocks + "  },\n" + tail
    doc = json.loads(new_raw)
    PATH.write_text(new_raw)
    print(f"added {len(missing)} entries; total {len(doc['strings'])}")

if __name__ == "__main__":
    main()
