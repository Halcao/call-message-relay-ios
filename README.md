# CallRelay

<img src="CallRelay/Resources/Assets.xcassets/AppIcon.appiconset/AppIcon.png" width="128" alt="CallRelay 应用图标" align="left">

<br>

**把你的手机变成自己蜂窝模块的随身终端。** CallRelay 让你把 SIM 卡与蜂窝模块插在自有
Linux 网关上，然后在 iPhone / iPad 上用**系统通话界面**接打这卡的电话、收发短信、
管理通话历史。业务数据由你自己的网关处理，作者不运营通话中转或推送服务。

本仓库包含完整项目：

- **iOS/iPadOS 客户端**（`CallRelay/`，Swift/SwiftUI，iOS 17.0 起）；
- **自托管网关**（`gateway/`，Go；由 [CellBridge](https://github.com/mccding/CellBridge)
  派生并扩展为多线路统一网关，上游 MIT 许可与归属保留）；
- **可移植构建工具**（`tools/`）与文档（`docs/`）。

界面默认简体中文，内置简体中文 / 繁体中文 / 英文三语；跟随系统浅色 / 深色外观。

## 产品功能

- **系统通话体验**：iOS 17.4+ 使用 Apple 原生 LiveCommunicationKit 上报来电、展示系统
  通话界面（不是第三方 LiveKit）；iOS 17.0–17.3 自动回退 CallKit。拨出、接听、拒绝、
  挂断、静音、DTMF、听筒/扬声器/蓝牙路由均由系统接管；「已接通」以网关 `active` 且
  媒体连通为准，接口成功不冒充接通。
- **短信**：按号码聚合的对话、气泡界面、新建短信、自动已读；发送状态如实显示网关的
  排队 / 已提交 / 已发送 / 失败。**「已提交」只代表交给网关，不承诺运营商最终送达。**
  临时断网时保存待发任务，恢复连接后重试；重启后结果不确定的发送需要手动确认，避免重复发出。
- **多卡多线路与多设备授权**：网关可挂多条蜂窝线路；每个配对密钥按线路获得**接收来电 /
  接收短信 / 发送短信 / 拨打**等独立权限，可标记「全部线路」或「管理号码」。默认拨出
  线路持久保存、按次临时切换，绝不静默换卡。App 内置网关管理台：查看/新建/撤销密钥与
  设备、管理线路与号码、语音信箱与录音库、脱敏诊断（需支持管理 API 的网关版本）。
- **通讯录**：读取系统通讯录（含 iCloud 联系人，授权后才读），拨号与短信支持姓名/号码
  联想；**非破坏性去重导出 vCard**（系统通讯录不被修改）；可把联系人**上传同步到网关**
  （仅新增/更新、绝不删除；按同步主体隔离），供网关侧解析姓名与白名单使用。
- **通话历史**：本机持久缓存最近通话与短信（超出上限如实标注），条件式刷新；支持
  **批量标记已读 / 批量删除**，按网关隔离的删除墓碑保证重启后不复活。
- **语音信箱、通话录音、会议**：网关语音信箱收件、播放、删除；1:1 通话与会议的手动
  录音（网关侧存储，受网关保留策略约束）；多通电话合并会议与分线保持（以上能力以所用
  网关版本的实际支持为准）。
- **骚扰拦截**：本机可编辑规则（号码/前缀/关键词/正则/信任项 + 默认关闭的中文/英文
  预设，对验证码类通知提供保护），命中短信进可恢复的隔离区；来电名单精确匹配，可
  仅标记或系统上报后立即结束。兼容网关还支持**按密钥的网关侧拦截**，两台设备共享一份
  策略、不同密钥互相隔离。内置 2020 年中文历史名单为 Apache-2.0 归档，默认关闭。
- **快捷指令 / Siri**：三个 App Intents——「用 CallRelay 拨打」「编写短信」「打开
  CallRelay（拨号/短信/语音信箱）」；保存的线路失效时保留草稿并要求重选，绝不退化。
- **iCloud 同步（可选）**：同一 iCloud 账号下同步短信、通话历史与拦截规则，按网关隔离；
  数据在 CloudKit 中加密存储，不作端到端加密承诺。每台设备的私钥与访问令牌保持独立，
  配对恢复凭据可通过 iCloud 钥匙串同步。需要登录个人 iCloud 账号，并使用具有匹配
  CloudKit 容器权限的签名；账户或权限不满足时会显示不可用。配置见 [BUILD.md](BUILD.md)。
- **音频自动选路**：手机与网关之间的网络跳在 **直连 WebRTC、QUIC 直连与认证 WSS 中继**
  三条路径间按真实测量评分自动选择（也可手动强制）。网络跳协商 **Opus（16–64 kbps，
  带内 FEC 与闭环码率自适应）**，与旧客户端互通时回退 PCMU；蜂窝模块到运营商的一段
  仍是传统 **8 kHz 窄带电话音质**，整条链路不是宽带 HD。直连并不必然优于中继；通话中
  断链进入有界恢复窗口（45 秒内以真实媒体帧为准判活），恢复不了才如实结束，绝不自动
  重拨。不承诺零延迟，也不承诺弱网完全无中断。
- **推送与后台来电（全部自托管）**：作者不提供推送中转服务，也不分发 APNs 密钥；
  第三方网关不能借用作者 App 的 APNs 身份。原生后台来电的前提是**与实际签名团队 / App ID
  匹配的自有 APNs 配置**：App 把 PushKit 令牌注册到你自己的网关，由网关用你自己的密钥
  直接发送。PWA 方案则是每个网关在自己域名自托管 VAPID 标准 Web Push，**用户点通知后**
  打开网页处理来电——不等同于后台直接弹系统电话。
- **演示模式**：完全离线的内存网关 + 保留的 555 模拟号码，不联网、不发短信、不触发
  真实来电，可先行走查全部界面与状态。
- **配对安全**：粘贴或扫码网关配对数据；Ed25519 私钥永远留在本机 Keychain；
  先匿名校验网关指纹再发证明；v2 注册密钥安装留存「恢复凭据」，重装或换机可在**你自己的
  iCloud 钥匙串**范围内重新注册而无需再碰控制台（可一键清除）。

## 适用人群

- 手里有蜂窝模块（如 DJI IG830 / Quectel QDC507 等）与一台常开 Linux 主机，想把实体
  SIM 变成全家可用的「云端手机」；
- 希望掌控通话与短信传输、存储位置的自托管用户；
- 开发者与研究者：本项目源码公开，可自行构建、审计与派生（许可证见文末）。

不适合：需要运营商级号码托管、99.9% 可用性承诺或开箱即用免部署的用户。

## 快速上手

1. **部署网关**：在常开 Linux 主机上构建并运行 `gateway/`（模块驱动、AT 与音频链路
   要求见 [docs/self-hosting.md](docs/self-hosting.md)；架构与协议见
   [docs/architecture.md](docs/architecture.md) 与 [PROTOCOL.md](PROTOCOL.md)）。
2. **生成配对数据**：在网关控制台（或 App 管理台）为一条或多条线路创建密钥，得到
   配对串/二维码（例如 `relay.example.com` 或测试环境 `192.0.2.10` 的配对载荷；正文示例
   联系人一律为张三/李四、号码一律为保留 555 段）。
3. **安装并配对 App**：从源码构建安装（见下），或使用公开未签名包（见「版本与安装」）；
   粘贴/扫码配对数据，授权通知与联系人后即可使用。

### 从源码构建（客户端 + 网关一览）

```sh
git clone https://github.com/JiangNanGenius/call-message-relay-ios.git
cd call-message-relay-ios
./Scripts/bootstrap.sh        # 拉取并校验固定校验和的 WebRTC M151 二进制
xcodegen generate             # 生成 CallRelay.xcodeproj
./Scripts/xcodebuild.sh build -project CallRelay.xcodeproj -scheme CallRelay \
  -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO

cd gateway && ../tools/go.sh build ./...   # 网关（按目标平台准备 Go、C 工具链与音频依赖）
```

完整步骤、缓存接口、测试、真机签名与 APNs/CloudKit 说明见 [BUILD.md](BUILD.md)。

## 版本与安装

- 本仓库**源码**当前版本为 **0.3.60（构建 71）**；上文功能以源码为准。
- GitHub **Releases 最新公开安装包为 v0.3.39（构建 46）预发布**：Feather 纯原生与
  App Store PWA 两个**未签名** IPA + `SHA256SUMS`。0.3.40–0.3.60 的新功能目前**只以
  源码形式发布，没有对应的公开 IPA**；本应用也**未上架 App Store**。
- 在 Feather「源」中添加（仅指向纯原生未签名版本，历史条目不改、不会指向不存在的资产）：

  ```
  https://raw.githubusercontent.com/JiangNanGenius/call-message-relay-ios/main/feather.json
  ```

- Feather 安装需要你用**自己的** Apple 证书与描述文件重签。并非任意重签都能获得全部
  能力：iCloud 同步与原生锁屏来电各需要与实际签名身份匹配的容器 / APNs 配置（自建时
  必要时改用你自己的 App ID 与 CloudKit 容器），重签成功也不代表推送或真实网关通话已
  验收。安装前提与配置步骤见 [BUILD.md](BUILD.md)。

## 测试与兼容性

构建与测试记录分为三个层面：

1. **自动化测试**：模拟器单元测试 + XCUITest，结果以本仓库 GitHub Actions 为准
   （个别历史时序敏感用例存在波动）；
2. **服务健康**：网关自身的健康检查只说明服务在跑；
3. **真机验收**：系统来电界面、PushKit/APNs、真实音频与运营商短信收发必须在签名真机 +
   真实网关上由使用者本人验收。README 与测试绿灯都不代表物理真机已通过。

## 文档地图

- [BUILD.md](BUILD.md) — 构建、缓存、测试、签名与 CloudKit/APNs 配置
- [docs/release.md](docs/release.md) — 双变体发布流程与私有签名边界
- [docs/self-hosting.md](docs/self-hosting.md) — 网关自托管部署
- [docs/architecture.md](docs/architecture.md) — 系统架构
- [docs/transport-quality.md](docs/transport-quality.md) — 音质/传输路径的工程事实
- [PROTOCOL.md](PROTOCOL.md) — 协议与安全设计
- [PRODUCT.md](PRODUCT.md) — 产品定义

## 许可与第三方归属

- 本项目原创代码（客户端与网关自有部分）适用 **PolyForm Noncommercial 1.0.0**，
  仅授权非商业用途，全文见 [LICENSE](LICENSE)。
- 网关基于 CellBridge（MIT）派生：上游 MIT 许可与归属声明随
  [gateway/THIRD_PARTY_NOTICES.md](gateway/THIRD_PARTY_NOTICES.md) 保留，上游代码**不**因
  本仓库的 PolyForm 许可而改变授权。
- 第三方组件（Google WebRTC、libopus、SpeexDSP、内置历史骚扰名单等）各自保留其许可证，
  见 [ThirdPartyNotices.md](ThirdPartyNotices.md)。
- HINLINK、DJI、Quectel、Apple 等商标仅用于兼容性描述，无隶属或背书关系（见
  [NOTICE](NOTICE)）。
