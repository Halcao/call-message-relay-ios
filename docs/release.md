# CallRelay 发布流程（双未签名变体；Feather 源纯原生）

每次更新同时发布两个**未签名**构建，共用同一营销版本与构建号；**Feather 安装源永远只
指向纯原生版**。本文件是公开、可复现的发布流程说明；已发布版本与其下载地址、校验和以
GitHub Releases 与 `feather.json` 实际内容为准。

## 现状（截至本次文档同步）

- 本仓库源码版本为 **0.3.60（构建 71）**。
- GitHub Releases **最新公开安装包为 v0.3.39（构建 46）预发布**（两个未签名 IPA +
  `SHA256SUMS`）；`feather.json` 的最高条目同为 v0.3.39 纯原生版。
- **0.3.40–0.3.60 尚未发布公开 IPA**，App 也未上架 App Store。发布流程在任何步骤都
  **不得**把 Feather 源、Release 链接或文档指向不存在的资产；已发布 tag 与资产视为
  不可变，历史 feed 条目不改。

| 变体 | Xcode 配置 | 桥接 | 渠道 |
| --- | --- | --- | --- |
| Feather 纯原生 | `Release`（默认） | 无网页推送桥接 | GitHub Release 未签名 IPA + Feather 源；私有签名 IPA 只交给设备所有者 |
| App Store PWA | `Release-PWA` | 自托管 PWA Web Push 桥接：`callrelay://` 来电检查回链 | GitHub Release 未签名 IPA；App Store 仅在单独明确授权后提交 |

桥接由编译条件 `PWA_BRIDGE` 与 `Info-PWA.plist` 隔离；默认构建不含网页推送 UI、
deeplink 或桥接字符串。PWA 桥接不含 Bark、不含中央服务、不含第三方通知依赖：每个网关
在自己已有的 HTTPS 域名上托管可安装 PWA（manifest + service worker），自持 VAPID 密钥
并向自己的浏览器订阅发送标准 Web Push（RFC 8030 + VAPID + RFC 8291 加密）。App 只生成
短时单次绑定码；浏览器订阅密钥只保存在网关。**PWA Web Push 需要用户点通知回到网页
处理来电，不等同于原生 PushKit/APNs 后台来电响铃**；原生推送由自托管网关用与 Bundle ID
匹配的 APNs 密钥发送，作者不运营中央推送。

## 工具

- `Scripts/package-release-variants.sh` — 构建两个变体（`CODE_SIGNING_ALLOWED=NO`）、
  按版本打包 IPA、逐个校验并生成 `SHA256SUMS.public.txt`。缓存通过
  `Scripts/build-env.sh` 自动跟随源码卷（可被 `CALLRELAY_CACHE_ROOT` /
  `DERIVED_DATA_ROOT` 覆盖）；`--app-native/--app-pwa` 可对已有构建产物只做打包+校验；
  `--output-dir` 默认 `build/feather-<版本>-<构建号>/`。
- `Scripts/verify-release-variant.sh native|pwa <.app|.ipa> [--version X] [--build N]` —
  验证单个产物：native 必须无 `CFBundleURLTypes`、无桥接字符串表、二进制无桥接符号；
  pwa 必须具备 `callrelay://` 与 `WebPushBridge.strings`；两者都必须未签名（无
  `embedded.mobileprovision`、无 `_CodeSignature`）；版本号/构建号可选断言。
- `Scripts/update-feather-source.py` — 从实际原生 IPA 生成/更新 `feather.json`：读取
  真实大小与 SHA-256，拒绝任何含桥接内容的 IPA；`--feed` 合并保留历史版本条目。
- `Scripts/sign-native-ipa.sh` + `Scripts/verify-signed-ipa.py` — **所有者侧私有签名**
  流程（见下节）。签名身份必须显式传入（`--identity` 或环境变量
  `CALLRELAY_SIGNING_IDENTITY`），缺失时脚本直接报错，绝不自动挑选钥匙串证书。
- `.github/workflows/build.yml` — 单元/UI 测试（明暗外观）、两个变体的无签名真机
  构建与身份校验，并运行 Feather 生成器做纯原生自检。CI 结果不代表真机验收。

## 公开发布步骤

1. 在 `project.yml` 同时更新 `MARKETING_VERSION` 与 `CURRENT_PROJECT_VERSION`
   （两个变体共用），提交到 `main`。
2. 构建、打包、校验两个变体：

   ```sh
   ./Scripts/package-release-variants.sh
   # 或对已有构建产物：
   ./Scripts/package-release-variants.sh \
     --app-native <native CallRelay.app> --app-pwa <pwa CallRelay.app>
   ```

   产物位于 `build/feather-<version>-<build>/`：
   `CallRelay-Feather-Native-<version>-<build>-unsigned.ipa`、
   `CallRelay-AppStore-PWA-<version>-<build>-unsigned.ipa`、`SHA256SUMS.public.txt`。
3. 生成/更新 Feather 源（只能使用原生 IPA；脚本会拒绝任何含桥接的包；**仅在该版本的
   Release 资产真实上传成功后**才提交指向它的源）：

   ```sh
   ./Scripts/update-feather-source.py \
     build/feather-<version>-<build>/CallRelay-Feather-Native-<version>-<build>-unsigned.ipa \
     --tag v<version> --feed feather.json --output feather.json
   ```

4. 提交 `feather.json` 与文档，打标签并推送：

   ```sh
   git tag v<version> && git push origin main v<version>
   ```

5. 创建 GitHub 预发布（与既有版本保持一致），只上传两个未签名 IPA 与校验和：

   ```sh
   gh release create v<version> --prerelease \
     --title "CallRelay <version> (<build>)" --notes-file <release-notes.md> \
     CallRelay-Feather-Native-<version>-<build>-unsigned.ipa \
     CallRelay-AppStore-PWA-<version>-<build>-unsigned.ipa \
     SHA256SUMS.public.txt
   ```

   上传时把 `SHA256SUMS.public.txt` 重命名为 `SHA256SUMS`，避免与本地文件混淆。
6. 发布后复核：未登录下载 `shasum -a 256 -c SHA256SUMS`；逐一打开 Feather 源中的
   下载地址，核对 `size`/`sha256`/`version` 与实际资产一致；确认源中**没有**指向未
   发布版本的条目。

## 私有签名（仅设备所有者）

- 私有签名原生 IPA 用 `Scripts/sign-native-ipa.sh` 生成，签名身份、描述文件与团队
  标识**全部来自所有者自己的钥匙串/开发者账号**，通过显式参数或环境变量传入；仓库与
  公开渠道不含任何签名材料。
- 产物交付只走所有者控制的私有位置（本地目录/私有云盘），latest-only 并保留上一版
  回滚包；交付后用 `Scripts/verify-signed-ipa.py` 复核身份 entitlements。
- **私有签名包不是公开认证**：它不构成「稳定版」声明，也不代表真机验收完成。

## 安全红线

- GitHub 公开资产只能包含两个未签名 IPA 与校验和：不得含 `embedded.mobileprovision`、
  `_CodeSignature`、`.p12`/`.p8`、描述文件、设备列表或签名身份；发布前运行
  `Scripts/verify-release-variant.sh` 验证。
- Feather 源只能指向纯原生未签名 IPA；App Store/TestFlight 上传需要单独的明确授权。
- 已发布版本与其标签/资产不可变：下一版本使用新的补丁版本号，不覆盖旧资产，不删除
  历史 feed 条目。

## 版本记录与诚实边界

- 逐版本变更以 GitHub Releases 的发布说明与 `feather.json` 为准；本文档不再维护内嵌
  的现场排障记录（历史版本曾包含的私人实测细节已在本次文档同步中移除）。
- 发布=公开**未签名**构建。自动化测试（Actions）、网关服务健康与**真机验收**是三层
  不同状态：系统来电界面（LiveCommunicationKit/CallKit）、PushKit/APNs 后台来电、
  真实通话音频与运营商短信送达，均须由使用者在签名真机 + 真实网关上自行验收。
