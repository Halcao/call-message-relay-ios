# 协议与证据边界

本文件描述 CallRelay **当前源码**实现的线缆协议边界。客户端的主目标是统一网关
**v2（`/api/v2`）**；对固定参考 **CellBridge v2.0.6**（提交
`2ee9d6b05d8a4391f782e6ce8a6d23ef57015fda`）的 **v1** 路径保留兼容。下列约定来自直接
阅读客户端 `CallRelay/Networking/` 与网关 Go 处理器，而非任何生成文档。

**本仓库不提供覆盖完整 v2 的 OpenAPI 规范，也不做超出源码的协议承诺。** 逐字段的精确定义
与全部边界以源码为准：客户端类型在
[`CallRelay/Networking/APITypes.swift`](CallRelay/Networking/APITypes.swift) 与
[`CallRelay/Networking/V2Wire.swift`](CallRelay/Networking/V2Wire.swift)；网关路由与处理器在
[`gateway/internal/unified/api/server.go`](gateway/internal/unified/api/server.go)；整体设计见
[`docs/architecture.md`](docs/architecture.md)。

本文档中的 `https://gw.example.com` 均为示例主机名，不代表任何具体部署。

## 版本选择与通用约定（v1/v2 共用）

- 基址 `https://{gateway}/api/{version}`；客户端按绑定载荷中的 `apiVersion` 选择版本段，
  缺省为 v1；版本段只接受规范化 `v1|v2`，恶意值（如 `v2/../admin`）拒绝。
- 业务请求使用 `Authorization: Bearer <accessToken>`；401 时串行化、只刷新一次再重试原
  请求；刷新被拒即清除凭据并如实上报未授权，瞬时 5xx 保留凭据待重试。
- 时间单位：实体 `createdAt`、通话、事件、录音时间为 Unix **毫秒**；v1 配对
  `pairing/start.expiresAt` 为 Unix **秒**；`ice` 的 `expiresAt` 为 **RFC3339 字符串**，
  可能不含小数秒。
- 变更类请求使用 `Idempotency-Key` 头（网关要求非空、≤200 字符）；重放命中同一已完成
  操作时返回原响应并带 `Idempotency-Replayed: true`。
- 列表 GET 可返回强内容 **ETag**（响应字节 SHA-256 前缀，`"sha256-…"`），客户端带
  `If-None-Match` 得 304，用于保留本地作用域缓存；`Cache-Control: no-store` 保持，缓存
  是应用层自有副本，不是中间缓存的。
- 事件信封 v1/v2 完全一致：`{id, seq, type, createdAt, data}`，`type` 含
  `line.updated / call.incoming / call.updated / call.ended / message.created / ...`；
  通话状态词汇 `idle / incoming_ringing / outgoing_dialing / answering / connecting /
  active / held / ending / ended / voicemail(_recording) / failed`，未知值与缺省一律映射为
  客户端 `recovering` 而非崩溃或丢弃。

## 统一网关 v2（当前主路径）

JSON 全 camelCase（v1 遗留的 `gatewayID`/`lineID` 大写仅在 v1 段适用）。鉴权仍是
Bearer；`POST /api/v2/auth/refresh` 请求 `{refreshToken}`，响应含 `accessToken`、
`refreshToken` 与 `deviceId`（与 v1 不同，v2 直接回带 deviceId）。

### 匿名发现与注册

- `GET /api/v2/identity` →
  `{gatewayId,gatewayName,apiVersion:"v2",mode:"unified",publicKey,fingerprint}`，
  `fingerprint` 形如 `sha256:<hex>`。配对前与每次建立凭据连接前都用它校验端点与
  gatewayId/指纹绑定；不匹配即阻断所有带令牌路径。
- `GET /api/v2/health` → `{status,version,apiVersion:"v2",lines,time,push:{configured,
  environment?}}`：推送**就绪性**是匿名健康面的一部分，客户端借此区分“已登记令牌”与
  “网关真的能发”，并把环境与自身签名环境比对后才做锁屏来电声明。
- `POST /api/v2/enroll`（匿名）请求
  `{enrollmentKey,deviceName,devicePublicKey,proof}`；`enrollmentKey` 为控制台一次性
  `key_xxx.<secret>`（按**首个**点拆分），证明消息精确为
  `keyId + "\n" + secret + "\n" + gatewayId + "\n" + deviceName`（Ed25519，公钥/证明
  base64）。响应 `{deviceId,accessToken,refreshToken,gatewayId,gatewayName}`。
- 注册成功后客户端把 `{gatewayId,gatewayName,endpoint,fingerprint,enrollmentKey,
  defaultLineId}` 存为 Keychain 恢复凭据（`afterFirstUnlock` + 可同步；无 iCloud
  Keychain 权限时降级为本机存储并如实标注 deviceOnly，绝不声称已同步）。新设备/重装后
  先匿名校验 endpoint+gatewayId+指纹，再用**当前安装**的 Ed25519 密钥重新 enroll；
  服务端返回 `CB-ENROLL-REVOKED`（或 401/403）时标记该凭据 blocked，不再重试。
- 控制台配对载荷（二维码/JSON）：
  `{gatewayId,gatewayName,baseURL,enrollmentKey,fingerprint,apiVersion:"v2"}`。

### 密钥作用域与设备

- 授权模型：**密钥 → 线路**的逐线授权 `receiveSms / receiveCalls / sendSms / dial`，
  或密钥级 `allLines`；设备挂在密钥下。所有权限由**服务端强制**，客户端不自行放行，
  线路/线程/通话视图按访问者的授权作用域返回。
- `GET /api/v2/device` → `{device:{id,name,keyId,defaultLineId}, key:{id,name,allLines},
  lines:[AuthorizedLine], capabilities:{...}}`。`capabilities` 是特性声明映射（如
  `harassment / harassmentImport / recordingAuto / smsQuarantine / smsKeyOverlay /
  smsThreadActions`）：客户端按名特性检测而非探测 404，忽略该映射的旧客户端照常工作。
- `GET /api/v2/lines[?line=id]`；`PUT /api/v2/lines/{id}/number {phoneNumber}`（空串=
  重置）需**双重授权**：对该线路的实时 receiveSms 授权 + 密钥级 manage-number 能力，
  响应带 `canManageNumber`；**旧网关不回该字段时客户端解码为 false**。
- `PUT /api/v2/devices/{id}/preferences` 为**存在性 PATCH 语义**：字段出现才修改，
  可选项 `defaultLineId / uplinkDenoise / downlinkDenoise / autoRecord`；`defaultLineId`
  必须在密钥授权线路上，否则 403。`PUT /api/v2/devices/{id}/push` 路径 deviceId 必须
  等于令牌所属设备。

### 短信

- `GET /api/v2/threads[?line=id&folder=normal|junk|all]`：线程 `key` 为 `lineID:workerKey`
  前缀（`junk` 为按密钥隔离的隔离区，`all` 含两者）；
  `GET /api/v2/messages?threadKey=&before=&beforeId=&limit=` 返回时间升序页，是否还有
  更早消息看响应头 **`X-CallRelay-Has-More: true|false`**（客户端兼容旧 CellBridge
  头名）；`beforeId` 必须与 `before` 同现。
- `POST /api/v2/messages {lineId,to,body}` + `Idempotency-Key` → 201；单授权线路可省略
  `lineId`（否则 400 `CB-V2-LINE-REQUIRED`）。响应 `status` 可能是 `failed`（调制解调器
  落库后拒发），客户端作为真实失败消息展示；同一逻辑提交固定用同一幂等键重试，已落库
  `failed` 的消息改用**新**幂等键重提交。
- `POST /api/v2/messages/{id}/read` → 204；id 可含 `:`，路径段保持原样。
- **批量线程动作**（capability `smsThreadActions`）：
  `POST /api/v2/threads/actions {actions:[{id,kind:"read"|"delete",threadKey,
  throughCreatedAt,throughID}]}`（1–100 条），响应 `{results:[{id,ok,error?{code,
  message,retryable}}]}` **逐条**给出成败与可重试性，客户端据此落持久墓碑/已读状态，
  绝不把整批当原子成功。

### 通话、会议、语音留言与录音

- `GET /api/v2/calls?limit=&active=true&line=`（limit 1–500，`active=true` 为权威活跃
  过滤）；`POST /api/v2/calls {lineId,to,clientCallId}` + 幂等键 → 201。
- 动作 `POST /api/v2/calls/{id}/{action}`：`answer|reject|decline|hangup|hold|resume|
  dtmf`（均带幂等键；`dtmf` 体 `{digit}`）。
- v2 通话对象用 `lineId`（小 d）与可选 `held/conferenceId/ownerDeviceId`，客户端解码
  映射回共享 `CallRecord`。
- WebRTC 协商：`POST /api/v2/calls/{id}/webrtc/offer {sdp,type:"offer",trickle?}`
  （**无 transport 字段**；`trickle` 仅当网关声明 `capabilities.iceTrickle` 且本地 offer
  确实增量产生时携带，缺省/false 保持完整-SDP 旧行为）→
  `{sdp,type:"answer",iceMode,attachId?,candidateCursor?,gatheringComplete?}`。
- **trickle ICE（当前实现）**：声明 `iceTrickle` 后，候选经
  `POST/GET …/webrtc/candidates`（offer/probe/preflight 三个面各有对应端点）按**批次**
  增量交换：请求 `{attachId,candidates[]}`（attach 令牌围栏，被替换协商的批次按过期拒
  绝；网关限每批/每 attach/每行长度上限），POST 应答 `{added}`，GET 轮询返回网关侧新
  候选 `{candidates,cursor,complete,truncated?}`。客户端在未声明该能力时**必须**回退
  完整 SDP。
- ICE 配置 `GET /api/v2/calls/{id}/ice` 与设备级 `GET /api/v2/ice` 同形状：
  `{policy,iceServers,mediaTransports:["ice","ws"(, "quic")], capabilities:{audioDenoise:
  ["uplink","downlink"], mediaStandby:true, iceTrickle:true, mediaQuic?, quic?}}`。
  `iceServers` 的 TURN 凭据是**可选短期凭据**：`mediaTransports` 含 `"ws"` 时，认证 WSS
  中继本身就是兜底路径，客户端不把 TURN 当作必需依赖。
- WSS 媒体面 `GET /api/v2/calls/{id}/media`（升级前鉴权）：先文本 `{"type":"ready",…}`
  控制（通告协商 codec/`fmt=seq16`、transport 角色与 epoch），随后二进制帧=每 20 ms 一
  帧（PCMU 恰好 160 字节；Opus 为 `[seq u16 BE][payload]`，见
  `docs/transport-quality.md`）；文本 `{"type":"ping","buf","gap"}` ↔ `{"type":"pong",
  "t","buf","ugap"}` 承载拥塞证据。`standby` 角色参数仅在 `mediaStandby` 声明时有效。
- 直连探测与接管：`POST /api/v2/media/probe`（设备级、与呼叫无关的预飞探测，不建通话
  不采音频）`+ /renew + /candidates + DELETE`；呼叫级 `POST /calls/{id}/media/probe` +
  `/commit {preflightId?,standby?}` + `DELETE`；`GET /calls/{id}/media/state`；
  `GET /calls/{id}/media/measure` 为**仅测量**的 ping/pong RTT 面，绝不承载音频、不替换
  主会话。热备升权 `POST /calls/{id}/media/standby/promote {transport,token,expectEpoch,
  nonce}` → `{epoch,currentEpoch?,primary,standby?,…}`：令牌围栏 + 期望 epoch + 幂等
  nonce，被插队的换权在重放时可见可调和。
- QUIC 直连（当前实现，按能力启用）：`GET /api/v2/media/quic` →
  `{enabled:false}` 或能力+钉文档 `{version,gatewayId,algorithm,spkiHash,notBefore,
  notAfter,signature,identityKey,identityFingerprint}`（时刻为 Unix **秒**）+
  `candidates[] + stun[]`。客户端用**既有配对身份**验证钉文档上的 Ed25519 签名（网关
  `internal/unified/quic/cert.go: pinCanonical` 规范形），通过才钉 SPKI-SHA256 进 QUIC
  TLS 校验；签名不匹配在**任何 QUIC 拨号之前**硬失败，从无 trust-all。
  `POST /api/v2/media/quic/warm` 发放单次预热探测授权（NAT 打洞+真实可达/RTT 证据，不
  执行任何呼叫动作）；`POST /api/v2/calls/{id}/quic/candidate {candidate,networkEpoch}`
  完成每呼叫候选交换并返回单次打洞令牌与网关当前候选（候选必须是字面公网 `ip:port`；
  回环/私有/保留地址仅在网关显式开启 LANPunch 时允许）。QUIC 面只承载音频数据报，全部
  关键控制（bind/ready/promote/denoise/bye/ping）仍在既有认证 WSS 平面上，由
  call+device+epoch 三方相关。
- 会议：`POST /api/v2/conferences {callIds}`、`GET …/{id}`、`POST …/{id}/close`、
  `POST …/{id}/webrtc/offer`、`GET …/{id}/media`（**会议媒体不支持 standby**：携带该
  参数返回 409）、`POST …/{id}/split {callId}`、`POST …/{id}/legs/{callId}/{action}`、
  `POST …/{id}/recording/start|stop`。
- 语音留言：`GET /api/v2/voicemails[?…]`、`GET …/{id}`、`GET …/{id}/audio`（Bearer，
  WAV 字节）、`DELETE …/{id}`（404 视为已收敛的成功）。
- 录音：1:1 `POST /api/v2/calls/{id}/recording/start|stop`；`GET /api/v2/recordings`、
  `…/{id}`、`…/{id}/audio`、`…/{id}/waveform`、`GET /api/v2/recordings/settings`、
  `DELETE …/{id}`；自动录音能力 `recordingAuto` + 设备偏好 `autoRecord`。

### 事件流与恢复语义

- `wss://<host>/api/v2/events?after=<seq>`（Bearer；PWA 浏览器会话可用受限 cookie 面，
  跨站 Origin 被拒）。服务端约 25 秒 WebSocket ping，客户端 pong 顺延读截止；断线按指数
  退避+抖动重连并用 `?after=<已应用最大 seq>` 续传，**v2 没有全局 sync 游标，事件流
  `after` 是唯一恢复机制**，`/sync` 在 v2 是空操作（不发网络请求）。
- 授权被吊销时服务端以策略违规关闭连接，客户端停止重连风暴并如实呈现未授权。
- 通话“真正接通”的判定不变：网关 `active` 且媒体面就绪（新鲜媒体帧）；REST 201 不算
  接通，`.connected`/RTT 也不算。

### 其他 v2 面（要点级）

- 骚扰拦截 `harassment`（settings/entries/lists import/history/test）：网关侧数据按
  **配对密钥作用域**隔离，同密钥多设备共享、不同密钥互不可见。
- 通讯录 `contacts`（list/sync GET+POST/import/export/PUT/DELETE）：同步是 upsert-only，
  按主体（principal）作用域隔离，网关绝不因客户端同步删除线路侧数据。
- 设备级通知：`GET/PUT /api/v2/devices/{id}/bark` + `bark/test`（可选 Bark 第三方通知
  集成：服务地址 + 设备密钥；`PUT`/`GET` 只回传密钥已配置状态与提示片段，绝不回显明文）。
- PWA Web Push 面（`webpush/*`：`vapid` 公钥、`bind-token`、session cookie 作用域的
  subscribe/notify-mode/test/handoff/revoke）：自托管 VAPID，推送由部署自身持有密钥，
  不存在作者集中推送服务。
- `GET /api/v2/notifications/status`：推送登记与实际可发性的区分视图。

## v1 CellBridge 兼容层（遗留路径）

对固定参考 CellBridge v2.0.6（提交 `2ee9d6b0…`）的 v1 行为**保持原样**，直接阅读网关
Go 处理器核验：

- 基址 `https://{gateway}/api/v1`；通话与消息响应字段大小写为 `gatewayID`/`lineID`，
  其余 camelCase；`/calls` 列表恒为 JSON 数组。
- `pairing/start` 仅本地（loopback/私网/Tailnet），返回一次性密钥与网关公钥指纹；证明
  消息精确为 `pairingId + "\n" + oneTimeSecret + "\n" + gatewayId + "\n" + deviceName`
  （Ed25519）；成功返回 `{deviceId,accessToken,refreshToken}`。
- `/auth/refresh` 响应**只含** `accessToken` 与 `refreshToken`（无 deviceId），客户端保留
  原 deviceId。
- WebRTC（v1 基线）：参考实现只注册 **PCMU（G.711 µ-law）/8000，单声道，PT 0**；**非
  trickle**，一次性完整 offer/answer；tailnet/remote 为 relay-only ICE，TURN 短期凭据来自
  `/calls/{id}/ice`。
- 短信：`GET /messages?after&limit` 的 `after` 为 `sync_seq` 游标（时间升序、`limit`
  1–500）；`GET /messages?threadKey=&before=&beforeId=&limit=` 为单对话最新一页。消息字段
  `id, gatewayID, lineID, threadKey, direction, peer, body, encoding(gsm7|ucs2), status,
  createdAt`，状态机 `queued → submitted → sent`（失败 `failed`，入站入库即 `sent`，
  `read` 仅由读接口推进；`delivered` 只在枚举中预留）；`GET /threads` 为裸数组
  `{key, peer, unreadCount, lastMessage}`。
- 短信发送门槛来自 `/line`：`sim=ready` 且 `registration=registered` 且 **`sms=ready`**
  （AT 适配器正常轮询即上报，不能只看 `voice`）。同一逻辑提交（含网络失败后的重试）固定
  使用同一幂等键，网关重放已完成响应；对已落库 `failed` 的消息改用**新**键重提交。
- 分页头为旧名 `X-CellBridge-Has-More`（客户端 `X-CallRelay-Has-More ??
  X-CellBridge-Has-More` 兼容读取）；502 且体为 `messageResponse(status=failed)` 的拒发
  形态按真实失败消息展示。
- 事件：入站/发送完成发布 `message.created`，协议枚举还保留 `message.updated`，客户端
  两者都按消息对象合并；`WS /api/v1/events`（bearer）与 v2 共用同一信封与去重语义。
- `/sync`：`SyncResponse{from,to,hasMore,changes[]}`，`changes[]` 仅含
  `{seq,entity_type,entity_id,op}`；快照对账走一次有界 `GET /messages?after=0&limit=200`
  补水，按消息 `id` 幂等合并。
- VoIP 推送信封：`{callUUID, callId, handle, gatewayId, issuedAt}`（issuedAt 秒）；
  `callUUID` 与 `callId` 同为网关呼叫 id，**不保证合法 UUID**，客户端用确定性 UUIDv5
  收敛到同一系统通话。

## 本地过滤、通讯录与云同步（非网关协议）

- 垃圾规则、名单、信任号码、待发短信**默认只存本机**：规则为 JSON 文件，待发短信以
  网关标识哈希做作用域、受数据保护的 outbox 文件；两者都不含凭据，名单 HTTPS 拉取不带
  任何鉴权头、不上传统计。仅当拥有者显式启用可选 CloudKit 同步时，垃圾规则文档、信任
  号码与名单**设置**（不含名单号码本身）才会复制到同一 iCloud 账号的其他设备；
  可执行的待发 outbox、配对私钥与网关令牌始终只留在本机，任何账号设置下都不同步。
- 联系人经系统 `CNContactStore` 读取（统一联系人天然包含系统 iCloud 联系人），导出使用
  `CNContactVCardSerialization`，全程不修改系统通讯录。
- 可选 iCloud 同步使用 CloudKit **私人数据库**自定义 zone，正文/号码/姓名与规则/名单
  设置的 JSON 全部写入 `CKRecord.encryptedValues`（CloudKit 服务端加密存储，属于加密
  at-rest；并非在所有账号设置下都保证端到端加密），记录名为 `entity|logicalID`（首个
  `|` 分隔，逻辑 id 可含 `.`；逻辑 id 带 `<scope>.` 前缀，不同网关相同原始 id 不冲突），
  按网关标识 SHA-256 前缀做 scope 隔离；删除始终复制为独立的**墓碑记录**
  （`SyncTombstone|entity|logicalID`，实体感知、可复制、按 deletedAt LWW），物理删除仅
  为尽力清理且不作为 ACK 依据；同步每轮**先拉后推**，按归档记录的 change tag 做乐观
  保存（`.ifServerRecordUnchanged`），冲突返回 serverRecordChanged 由纯函数收敛，增量
  token 过期自动全量重拉并重置基线。基线不声明 CloudKit 能力，启用前先解析
  embedded.mobileprovision（精确匹配配置容器且声明 CloudKit 服务），再在 ObjC
  `@try/@catch` 保护下用 `CKContainer.accountStatus`/`fetchUserRecordID` 实测生效签名
  权限和账号身份（描述文件权限可能宽于实际签名权限；不用 iCloud Drive 的
  ubiquityIdentityToken 判断 CloudKit 登录），未配置时只显示不可用、不触碰 CloudKit。

## 安全实现

- 端点校验：默认强制 HTTPS；仅在显式调试开关下允许 `localhost/127.0.0.1/::1` 的 HTTP；
  拒绝非 http(s) scheme、URL userinfo、fragment。无全局 ATS 例外。
- 重定向：仅允许同主机/同端口/同协议跟随，跨源或降级到 HTTP 时取消，避免 Bearer 泄露。
- 身份绑定：配对前与每次建立凭据连接前，先用**匿名** `/identity` 比对 gatewayId 与
  公钥指纹；不匹配则阻断所有带令牌的 REST/WS 路径并提示重新配对。
- 私钥：设备 Ed25519（CryptoKit `Curve25519.Signing`）原始密钥存 Keychain，
  `afterFirstUnlockThisDeviceOnly`。Ed25519 不能进 Secure Enclave，故不做该声明。
- QUIC 证书信任：见上文 v2 段——配对身份签发的 SPKI 钉文档，验签失败在任何拨号前硬
  失败；不存在 trust-all。
- Token：access/refresh 存 Keychain；401 时串行化、只刷新一次再重试原请求；不盲目重试
  计费/变更命令；事件流是只读流，断线按指数退避+抖动重连。
- 日志：只记录操作类别、状态码与安全错误码，不写号码、SDP、token、密钥、完整呼叫 id
  或路径；关联使用不可逆短哈希。

## CallKit / PushKit

- 用确定性 UUIDv5 把任意网关呼叫 id 映射到 CallKit UUID；已是 UUID 时原样使用。
  推送、事件、REST 轮询与重放因此收敛到同一个系统通话，避免重复来电。
- VoIP 推送在 await 报告 CallKit **之前**先占位去重；`mustReport` 为真但负载损坏/异网关/
  过期时，报告一个立即结束的最小占位通话以满足 PushKit 合规，且在报告+结束后才调用
  completion。绝不把占位伪造成真实通话。
- 接听：CXAnswerCallAction 的 fulfill/fail 只取决于网关 `/answer` 是否接受；媒体在其后
  异步建立，避免等待 `didActivate` 形成环形等待；媒体失败会结束网关通话与系统通话。
- 代次（generation）保护：挂断/重置/切换模式会令在途 dial/offer/answer 的迟到结果失效；
  若取消时网关可能已建立通话，则用同一呼叫做补偿性 hangup；provider reset 也尽力收敛
  网关。
- 真实“已接通”要求网关 `active` 且媒体路径建立；REST 201 不会显示已接通。

## 音频质量边界（当前实现）

- 网络段编码当前为 **Opus**：WSS 中继（8 kHz 窄带单声道、16–64 kbps 闭环自适应、
  seq16 成帧）与直连 WebRTC / QUIC 数据报（复用同一套编解码与调度机制），**PCMU/G.711
  为线缆兼容回退**（旧端或旧网关）。trickle ICE 与 QUIC 直连均按能力声明启用。
- 调制解调器 → PSTN 段固定 **G.711 8 kHz**，因此端到端可闻带宽仍是窄带；网络段换码
  改善的是丢包恢复与拥塞行为，不是宣称宽带音质。QDC507/运营商侧 16kHz 宽带
  （AMR-WB）**尚未验证**，整链不承诺 HD 音质。
- 详细的路径差异、拥塞响应、RTT 与媒体队列的区分、自动选路与 45 秒有界恢复、降噪开关
  默认值，见 [`docs/transport-quality.md`](docs/transport-quality.md)。
- UI 仅显示真实可测的粗粒度连接质量（连接状态、测量 RTT、标注为估计的队列时延），不做
  无依据的 DSP/延迟承诺。

## 验证边界

- 上述线缆行为由**合成测试**证明：客户端 XCTest（含本地脚本化 HTTP/WS 假服务）与网关
  Go 单测/集成测试覆盖编解码、协商、候选批次、幂等、授权作用域、事件游标与恢复状态机。
- 某个具体部署的端到端可听质量、弱网选路表现、锁屏推送唤醒、回声/蓝牙/扬声器与
  Wi-Fi/蜂窝切换**仍须现场验收**；本文档不为任何真实网络条件背书“直连一定更快”“零
  断流”之类的结论。自托管部署的验收方法见 `docs/self-hosting.md`。
