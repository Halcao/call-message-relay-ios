# 构建与运行

CallRelay 仓库包含 **iOS/iPadOS 客户端**（XcodeGen 工程 + 固定校验和的 WebRTC M151
二进制）与 **自托管 Go 网关**（`gateway/`）。本文覆盖客户端构建、测试、签名与网关构建
入口；网关部署细节见 [docs/self-hosting.md](docs/self-hosting.md)，架构见
[docs/architecture.md](docs/architecture.md)，发布流程见 [docs/release.md](docs/release.md)。

## 环境

- Xcode 26+（源码含按 `#available(iOS 26)` 门控的系统 API，编译需 iOS 26 SDK；
  部署目标 iOS 17.0。CI 固定使用同一稳定版本并在 workflow 中显式设置 `DEVELOPER_DIR`，
  本机路径请以实际安装位置为准）。
- [XcodeGen](https://github.com/yonaskolb/XcodeGen)（已验证 2.46.0）。
- 网关需要 Go 工具链；交叉编译到目标设备（如 linux/arm64）的配套工具见 self-hosting 文档。
- 若命令行 `xcode-select` 指向 Command Line Tools，请为每条命令设置 `DEVELOPER_DIR`，
  **不要**改动全局 `xcode-select`：

  ```sh
  export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
  export PATH="$DEVELOPER_DIR/usr/bin:$PATH"
  ```

## 缓存接口（可移植，自动跟随源码卷）

所有命令行构建都应通过 `./Scripts/xcodebuild.sh`（它 source
`Scripts/build-env.sh` 并注入缓存与并发参数，显式传参优先）：

- 缓存根 `CALLRELAY_CACHE_ROOT` 自动决定：源码位于外接卷（`/Volumes/<卷名>/...`）时，
  放在**该卷**的 `.codex-build/call-message-relay/<checkout 指纹>/`（不同 checkout 自动
  隔离；卷名中的空格仍需由工具正确处理）；源码不在 `/Volumes` 下时回退到仓库内
  `.build-cache/`。查看当前实际路径：

  ```sh
  ./Scripts/xcodebuild.sh --print-cache-path
  ```

- 该缓存目录承载 Xcode DerivedData、Go build/mod/tmp 缓存、打包临时文件；
  需要覆盖时用 `CALLRELAY_CACHE_ROOT`、`DERIVED_DATA_ROOT`，或对单次构建直接传
  `-derivedDataPath`。SwiftPM 使用同一缓存下的 `swift/`（`--scratch-path`）。
- 网关 Go 构建/测试通过仓库根的 `tools/go.sh` 包装（自动带上上述 Go 缓存环境），在
  `gateway/` 中执行 `../tools/go.sh build ./...` / `../tools/go.sh test ./...`。
- 直接调用系统 `xcodebuild` / `go` 不会加载此环境。签名包、发布产物、回滚材料与验收
  记录保存在项目目录，**不能**只放在缓存里。

## 1. 拉取并校验 WebRTC

```sh
./Scripts/bootstrap.sh
```

从上游 release 下载 `WebRTC-M151.xcframework.zip` 并按固定 SHA-256 校验（也是 SPM
校验和），通过后解压到 `Vendor/WebRTC/WebRTC.xcframework`（已被 `.gitignore` 忽略）。

> 上游 stasel/WebRTC tag `151.0.0` 指向的 release 资产当前返回 404；字节相同的 M151
> 二进制以相同文件名与校验和重发于 tag `151.0.1`。bootstrap 以校验和固定**确切的
> M151**，不静默升级。详见 `ThirdPartyNotices.md`。

## 2. 生成工程

```sh
xcodegen generate
```

产出 `CallRelay.xcodeproj`。Bundle ID 为公开应用标识 `com.jiangnangenius.callrelay`，
仓库**不含** `DEVELOPMENT_TEAM`，默认无签名构建。

## 3. 模拟器构建（无签名）

通用模拟器目标（无需先启动模拟器，适合无头校验/CI）：

```sh
./Scripts/xcodebuild.sh build \
  -project CallRelay.xcodeproj -scheme CallRelay \
  -configuration Debug \
  -destination 'generic/platform=iOS Simulator' \
  CODE_SIGNING_ALLOWED=NO
```

## 4. 逻辑测试

```sh
# 一步构建 + 在已启动模拟器上执行：
./Scripts/xcodebuild.sh test \
  -project CallRelay.xcodeproj -scheme CallRelay \
  -destination 'platform=iOS Simulator,id=<SIMULATOR_UDID>' \
  CODE_SIGNING_ALLOWED=NO
# 也可 build-for-testing / test-without-building 分步执行（见 xcodebuild 用法）。
```

用 `xcrun simctl list devices available` 获取 `<SIMULATOR_UDID>`。测试按
配对与 Keychain、网关线缆兼容与幂等、短信收件箱与发件箱、号码归一化、垃圾/骚扰规则、
退避重试与错误分类、通话路由与媒体恢复/门禁（WebRTC/QUIC/WSS 中继选择、45s 有界恢复、
本机媒体三态证明）、联系人去重与 vCard、CloudKit 同步（含描述文件解析与 ObjC 异常边界）、
App Intents、事件流、CallKit/LCK 上报语义等主题组织，用例清单见 `CallRelayTests/`。
另有独立的 **CallRelayUITests**（XCUITest）：以 `-callrelayDemoMode` 等启动参数进入
完全离线演示（保留的 555 合成号码），走查短信收发、垃圾隔离与恢复、规则预览并保存明暗
两种外观截图。编译通过与测试执行通过分别记录；模拟器启动失败不算测试通过。结果以
GitHub Actions（`.github/workflows/build.yml`）为准。

## 5. 真机构建（无签名 = 仅编译验证；签名 = 自己的证书）

```sh
# 仅验证可编译，不可安装：
./Scripts/xcodebuild.sh build \
  -project CallRelay.xcodeproj -scheme CallRelay \
  -configuration Release -sdk iphoneos \
  CODE_SIGNING_ALLOWED=NO

# 用你自己的团队与证书签名：
./Scripts/xcodebuild.sh build \
  -project CallRelay.xcodeproj -scheme CallRelay \
  -configuration Release -sdk iphoneos \
  DEVELOPMENT_TEAM=<你的TeamID> CODE_SIGNING_ALLOWED=YES
```

CallKit/LCK、PushKit、麦克风与 VoIP 后台模式都需要描述文件与签名才能在真机运行。

## 签名与 APNs（真机推送）

- 工程 entitlement 使用 iOS 的 `aps-environment`（带 `com.apple.developer.` 前缀的是
  macOS 键，iOS 不要用）。Debug 默认 `development`（APNs sandbox）；分发与生产网关需
  `production`，并在网关注册时上报匹配的 `environment`。
- **推送全部自托管**：App 把 PushKit VoIP 令牌注册到你自己的网关，由网关持有**你自己的**
  APNs 密钥（Bundle ID + Team 匹配）直接发送；本项目作者不运营中央推送服务，仓库与公开
  产物不含任何推送凭据。使用自有 Bundle ID 派生编译时，请配置你自己的 App ID、APNs
  密钥与网关，**不要**复用作者凭据。
- 私有签名打包使用 `Scripts/sign-native-ipa.sh`：签名身份必须**显式**提供
  （`--identity "<你的签名身份>"` 或环境变量 `CALLRELAY_SIGNING_IDENTITY`）；两者都未
  设置时脚本友好报错退出，**绝不**自动挑选钥匙串里的证书。它按 Bundle ID 与实际签名
  证书双重匹配选择描述文件，把描述文件的身份 entitlements 与仓库能力声明合并（仓库
  较窄取值优先、每个仓库键必须被描述文件授予），并用 `Scripts/verify-signed-ipa.py`
  复核产物确实携带 profile 派生的身份 entitlements。签名身份、Team、描述文件一律
  不进仓库与公开渠道。

### 可选 CloudKit 重签（iCloud 同步）

基线发布不含 iCloud 能力，App 内同步开关会安全显示「不可用」；这不是平台限制——用
**你自己的**开发者账号完成以下步骤即可启用：

1. 为该 App ID 勾选 iCloud → CloudKit，创建私人容器（如 `iCloud.<你的BundleID>`）。
   公开包的容器默认为 `iCloud.com.jiangnangenius.callrelay`（作者账号示例）；派生编译
   可用 UserDefaults 键 `callrelay.cloudSync.containerID` 指向你自己的容器。
2. 用包含 `com.apple.developer.icloud-services = CloudKit`、
   `com.apple.developer.icloud-container-identifiers` 与 APNs 的描述文件重签。客户端
   启用前做两道检查：解析 `embedded.mobileprovision` 须精确包含配置容器且声明 CloudKit
   服务；再在 ObjC 异常保护下实测 `CKContainer.accountStatus` / `fetchUserRecordID`
   的**生效**权限（描述文件可能比实际签名权限宽；权限缺失被转为「不可用」，绝不崩溃）。
   登录判断不使用 iCloud Drive 的 `ubiquityIdentityToken`。
3. 真实条件 = **个人 iCloud 账号已登录 + 签名授予容器权限**，两者缺一开关就如实不可用。
   `CKRecord.encryptedValues` 是 CloudKit 服务端静态加密（at rest），不是端到端承诺。
4. 缺少容器不影响本地短信/通话/规则/名单功能；联系人同步走系统「iCloud 联系人」与该
   容器无关。

## 演示模式

未配对可进入「演示模式」：完全离线的内存网关 + 保留的 555 模拟号码，不联网、不发短信、
不触发真实系统来电，仅用于走查界面与状态切换。

## 网关（简要）

网关是仓库内 `gateway/` 的 Go 服务（CellBridge 派生，多线路统一网关）。本地构建与测试：

```sh
cd gateway
../tools/go.sh build ./...
../tools/go.sh test ./...
```

目标设备交叉编译（CGO 与音频依赖）、配置样例、部署与回滚、硬件（模块/驱动/AT/UAC 或
ALSA）兼容性核对，全部见 [docs/self-hosting.md](docs/self-hosting.md)；协议与安全设计见
[PROTOCOL.md](PROTOCOL.md)；上游与第三方许可见 [gateway/THIRD_PARTY_NOTICES.md](gateway/THIRD_PARTY_NOTICES.md)。
iOS App 通过 HTTP/WebSocket/WebRTC/QUIC 与网关交互，不依赖具体主机型号或 CPU 架构。
