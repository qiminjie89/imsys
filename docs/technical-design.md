# IMSys 技术设计方案

> 面向 **直播房间 / 大规模在线 / 高并发广播** 场景的 IM 架构设计

---

## 一、项目背景

#### 目前问题
1. 客户端（Android/iOS）与后端之间缺乏fire-and-forget异步通信能力
2. 单房间频率限制(40?)，全局消息频率限制(200?)，缺乏快速扩容能力
3. 包体长度限制，目前无法解决
4. IM消息发送耗时150ms+，性能不太理想
#### 目标：
1. 建立客户端和后端的异步消息通路：上行支持客户端异步请求（即无需等待返回），用于业务的行为事件通知；下行支持后端推送消息广播/多播/单播
2. 更高的掌控度和灵活性，能快速扩容以应对大型活动，进一步提升消息发送频率（目标500qps），提高包体长度限制（20k）
3. 更低的消息发送耗时
4. 第一期目标：备用通道，应对腾讯IM限频的情况
#### 代价：
1. 运维成本（机器、人力、流量）
2. 稳定性风险
3. 引入新技术栈的代码维护成本

### 1.1 容量目标

**第一版目标：**
- 单房间最大用户数：10,000
- 系统同时在线用户数：100,000
- 单机房部署

### 1.2 设计原则

- 数据面高可用，控制面可快速恢复
- 不依赖强一致存储，追求 **工程可控性与可演进性**
- 能优雅应对：节点重启、扩缩容、网络抖动

### 1.3 关键约束

```
1. 用户同时只能在一个房间（user → room 是 1:1 关系）
2. Join/Leave 是用户行为，Resume 处理断线重连（用户无感知）
3. 纯内存设计，不使用 Redis 缓存（Join/Leave 高频）
4. 可接受短暂不一致，目标是系统可持续服务
```

---

## 二、系统架构

### 2.1 整体架构图

```
                        ┌─────────────────────────────────────┐
                        │         客户端 (Android/iOS)         │
                        └──────┬─────────────────────┬────────┘
                               │                     │
              1. HTTP 获取地址  │                     │ 2. WebSocket 双向通信
                               ▼                     ▼
                      ┌────────────────┐    ┌────────────────────────────┐
                      │   Dispatcher   │    │    Gateway (多实例)         │
                      │  - 负载均衡     │    │  ┌────────┐  ┌────────┐   │
                      │  - 健康检查     │    │  │连接管理 │  │协议编解码│   │
                      └────────────────┘    │  └────────┘  └────────┘   │
                                            │  ┌────────┐  ┌────────┐   │
                                            │  │Kafka   │  │本地扇出 │   │
                                            │  │消费/生产│  │Distribu│   │
                                            │  └────────┘  └────────┘   │
                                            └───────┬──────────┬────────┘
                                                    │          │
                                       gRPC Stream  │          │ Kafka
                                       (控制面)     │          │ (数据面)
                                                    ▼          │
                                         ┌─────────────────┐   │
                                         │   Roomserver    │   │
                                         │  (房间状态管理)  │   │
                                         └─────────────────┘   │
                                                               │
┌──────────────────────────────────────────────────────────────┼───────┐
│                            Kafka                             │       │
│  ┌─────────────────────┐              ┌─────────────────────┐│       │
│  │ im_biz_request      │              │ im_msg_broadcast    ││       │
│  │ (业务请求上行)       │              │ (消息广播下行)       │◄───────┘
│  │ Client→Gateway→这里 │              │ Python→这里→Gateway │
│  └──────────┬──────────┘              └──────────┬──────────┘
│             │                                    │
└─────────────┼────────────────────────────────────┼───────────────────┘
              │                                    ▲
              ▼                                    │
        ┌─────────────────────────────────────────────────────────┐
        │                    Python 玩法后端                       │
        │                                                         │
        │   消费 im_biz_request ──┬──► 业务处理 ──► 写 im_msg_broadcast
        │                        │                                │
        │                        └──► 维护消息 seq                 │
        └─────────────────────────────────────────────────────────┘

服务发现：Nacos（Gateway/Roomserver 注册，Dispatcher 订阅）
```

#### 数据流说明

| 方向 | 路径 | 说明 |
|------|------|------|
| **上行（请求）** | Client → Gateway → Kafka(im_biz_request) → Python | 客户端发送业务请求 |
| **下行（广播）** | Python → Kafka(im_msg_broadcast) → Gateway → Client | 后端广播消息到客户端 |
| **控制面** | Client → Gateway → Roomserver | Join/Leave/Resume 等房间操作 |

### 2.2 数据面与控制面分离

```
┌─────────────────────────────────────────────────────────────────┐
│  数据面（Data Plane）- 热路径，要求高可用                           │
├─────────────────────────────────────────────────────────────────┤
│  Python 后端 → Kafka → Gateway → Client                          │
│                                                                 │
│  特点：                                                          │
│    - Roomserver 短暂挂掉不影响消息推送                             │
│    - 减少一跳网络延迟                                             │
│    - 同一 room_id → 同一 partition → 保序                        │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│  控制面（Control Plane）- 相对低频，允许短暂中断                    │
├─────────────────────────────────────────────────────────────────┤
│  Client → Gateway → Roomserver                                   │
│                                                                 │
│  操作：JoinRoom / LeaveRoom / Resume                             │
│  特点：可重建，不要求强一致                                        │
└─────────────────────────────────────────────────────────────────┘

设计原则：控制面允许失忆，数据面必须坚挺
```

### 2.3 请求流向

| 场景 | 链路 |
|------|------|
| 业务请求（抽奖/送礼）| Client → Gateway → Kafka → Python |
| 房间操作 | Client → Gateway → Roomserver |
| 房间广播 | Python → Kafka (key=room_id) → Gateway → Client |
| 单播消息 | Python → Kafka (key=user_id) → Gateway → Client |
| 全局广播 | Python → Kafka (scope=platform) → Gateway → Client |

---

## 三、组件设计

### 3.1 Gateway（接入层）

#### 核心职责

- **数据面**：消费 Kafka，根据 localRooms/localUsers 过滤后推送给客户端
- **控制面**：房间操作（JoinRoom/LeaveRoom/Resume）→ Roomserver
- 客户端 WebSocket 连接管理（建连、心跳、断线检测）
- 轻量鉴权（JWT RS256，仅首次连接校验）
- 本地房间成员缓存（用于消息过滤和广播扇出）
- sendQueue + 背压控制

#### 数据结构

```go
type Gateway struct {
    gatewayID string  // 配置项：gateway.id
    ip        string
    port      int

    connections map[string]*Connection  // user_id → connection（不支持多端）
    distributors []*Distributor         // 按 hash(user_id) 分配连接
    localRooms map[string]map[string]bool  // room_id → Set<user_id>
    localUsers map[string]bool             // user_id → true

    roomserverStreams map[string]*grpc.ClientStream  // roomserver_id → stream
    knownEpochs map[string]uint64  // roomserver_id → epoch
    hashRing *ConsistentHash       // 路由预测

    kafkaConsumer *kafka.Consumer  // consumer_group = gateway_id
    userLocks sync.Map             // user_id → *sync.Mutex（请求串行化）

    status        GatewayStatus    // NORMAL / DEGRADED
    pendingLeaves []PendingRequest // DEGRADED 期间待补偿
}

type Connection struct {
    UserID      string
    WSConn      *websocket.Conn
    sendCh      chan []byte
    JoinState   JoinState  // INIT → PENDING → CONFIRMED
    RoomID      string
    TokenExpiry time.Time
    distributor *Distributor
    health      ConnHealth
}
```

#### JoinRoom 状态机

```
INIT ──JoinRoom请求──> PENDING ──收到ACK──> CONFIRMED
       (用户未加入)    (等待ACK)         (参与广播)

关键规则：
  - Gateway 必须收到 Roomserver ACK 后才更新 localRooms
  - 只有 CONFIRMED 状态才参与广播
  - BatchReSync 只上报 CONFIRMED 状态
```

#### 鉴权流程

```
1. 客户端建立 WebSocket，发送 AUTH{token}
2. Gateway 本地验证 JWT：签名(RS256) + 过期时间
3. 验证通过：绑定 user_id，启动过期检查定时器
4. Token 过期：断连（不触发 LeaveRoom，等待 Resume 超时）

JWT Claims: { sub: user_id, exp, iat, iss }
公钥来源：Nacos 配置中心
```

#### 路由预测

```
Gateway 发送请求时：hash(room_id) → 预测目标 Roomserver
Roomserver 收到后：归属本实例则处理，否则内部转发

设计要点：Gateway 只做预测，Roomserver 保证正确性
```

### 3.2 Roomserver（控制面服务）

#### 核心职责

- **仅负责控制面**（消息推送走 Kafka）
- 房间管理（用户进出房间、房间成员维护）
- 会话管理（用户与 Gateway 的映射关系）
- 用户断连状态管理（disconnected 超时处理）
- 内部转发（路由过时时，转发请求到正确节点）

#### 内部服务拆分

```
┌────────────────────────────────────────────────────────────────┐
│                        Roomserver                               │
├────────────────────────────┬───────────────────────────────────┤
│       RoomService          │          SessionSvc               │
│       (房间维度)            │          (用户维度)               │
├────────────────────────────┼───────────────────────────────────┤
│ • 房间生命周期管理            │ • 用户会话管理                    │
│   - CreateRoom/CloseRoom   │   - user → gateway 映射           │
│ • 房间成员管理               │   - user → room 映射              │
│   - room → users 映射       │ • 连接状态管理                     │
│   - room → gateways 映射    │   - 断线重连 (Resume)             │
└────────────────────────────┴───────────────────────────────────┘

数据结构独立，锁粒度独立，便于后续演进
```

#### 数据结构

```go
type RoomServer struct {
    epoch    uint64        // 启动时间戳（毫秒），用于检测重启
    status   ServerStatus  // RECOVERING / SERVING

    rooms map[string]*Room        // room_id → Room
    users map[string]*UserState   // user_id → state
    gateways map[string]*GatewayConn
    hashRing *ConsistentHash
    peers map[string]*grpc.ClientConn
}

type Room struct {
    RoomID   string  // MongoDB _id
    Members  map[string]*RoomMember
    Gateways map[string]*GatewayRoomView
    mu       sync.RWMutex
}

// 房间生命周期：Python 创建（开播），RoomClose（关播）
// 消息 seq：由 Python 维护，不在 Roomserver

type UserState struct {
    UserID    string
    RoomID    string     // 当前所在房间（1:1）
    GatewayID string
    Status    MemberStatus  // Online / Disconnected
}
```

#### 用户状态模型

```
               JoinRoom
  not_in_room ────────> online ←──Resume(重连)
                          │              ↑
                     连接断开        30-60s内
                          │              │
                          ▼              │
                    disconnected ────────┘
                          │
                     超时(60s)
                          │
                          ▼
                    not_in_room
```

---

## 四、通信协议

### 4.1 客户端 ↔ Gateway（WebSocket）

#### 消息帧格式

```
+----------+----------+----------+----------+------------------+
|  Magic   | Version  |  MsgType | BodyLen  |     Payload      |
|  2 bytes |  1 byte  |  4 bytes | 4 bytes  |   变长 (msgpack)  |
+----------+----------+----------+----------+------------------+

- Magic: 0x494D (ASCII "IM")
- Version: 0x01
- 字节序：大端（Big-Endian）
```

#### 推送消息体

```go
type PushPayload struct {
    Scope   string `msgpack:"scope"`    // "room" | "user" | "platform"
    RoomID  string `msgpack:"room_id"`  // scope=room 时必填
    UserID  string `msgpack:"user_id"`  // scope=user 时必填
    Seq     uint64 `msgpack:"seq"`      // Python 生成，用于保序
    Message []byte `msgpack:"message"`
}
```

#### 消息类型

```go
// 客户端 → Gateway
MsgTypeAuth       = 0x0001  // 认证
MsgTypeJoinRoom   = 0x0002  // 加入房间
MsgTypeLeaveRoom  = 0x0003  // 离开房间
MsgTypeResume     = 0x0004  // 断线重连
MsgTypeBizRequest = 0x0010  // 业务请求
MsgTypeHeartbeat  = 0x00FF  // 心跳（携带 room_id）

// Gateway → 客户端
MsgTypeAuthResp      = 0x1001
MsgTypeJoinRoomResp  = 0x1002
MsgTypeLeaveRoomResp = 0x1003
MsgTypeResumeResp    = 0x1004
MsgTypePushMessage   = 0x1010
MsgTypeHeartbeatResp = 0x10FF  // 包含状态检查结果
```

### 4.2 Gateway ↔ Roomserver（gRPC Stream）

```protobuf
message Envelope {
    uint32 msg_type = 1;
    bytes  payload  = 2;  // msgpack
    uint64 seq      = 3;
}

service RoomServerGateway {
    rpc Channel(stream Envelope) returns (stream Envelope);
}
```

#### 内部消息类型

```go
// Gateway → Roomserver
InternalMsgJoinRoom       = 0x2001
InternalMsgLeaveRoom      = 0x2002
InternalMsgResume         = 0x2003
InternalMsgBatchReSync    = 0x2004  // 崩溃恢复
InternalMsgDisconnect     = 0x2008  // 用户断连

// Roomserver → Gateway
InternalMsgJoinRoomAck   = 0x3001
InternalMsgEpochNotify   = 0x3004  // Epoch 变更
InternalMsgRoomClose     = 0x3005  // 房间关闭
```

### 4.3 错误码

```go
ErrCodeSuccess           = 0
ErrCodeRoomNotFound      = 2001
ErrCodeUserNotInRoom     = 2002
ErrCodeRoomClosed        = 2003
ErrCodeRoomMismatch      = 2004  // Resume 时 room_id 不匹配
ErrCodeOwnerUnavailable  = 3001  // Roomserver 内部转发失败
ErrCodeServiceRecovering = 5001
```

### 4.4 Kafka 消息格式

```json
// Topic: im_msg_broadcast
{
    "scope": "room",           // room | user | platform | multicast
    "room_id": "xxx",
    "user_id": "yyy",          // scope=user 时
    "user_ids": ["a", "b"],    // scope=multicast 时
    "seq": 12345,
    "priority": "normal",      // critical/high/normal/low
    "message": { ... }
}

// Partition Key: room_id（房间广播）或 user_id（单播）
// 每个 Gateway 独立 consumer_group，消费全部 partition，本地过滤
```

---

## 五、核心机制

### 5.1 ACK 驱动状态更新

```
JoinRoom:
  Client → Gateway → Roomserver
                         │ 处理成功
  Client ← Gateway ← ACK ┘
              │
        收到 ACK 后：
        1. PENDING → CONFIRMED
        2. localRooms[room].add(user)

断连（同步通知）:
  WebSocket 断开
      │
  Gateway 同步通知 Roomserver（3s 超时，重试 2 次）
      │
  更新本地状态：localRooms/localUsers.remove(user)
      │
  Roomserver：标记 disconnected，启动 60s 超时计时器
```

### 5.2 广播消息流程

```
Python 后端:
    1. 生成 seq（房间级单调递增）
    2. 写入 Kafka: { scope:"room", room_id:X, seq:N, message:M }
    ↓
Kafka (同一 room_id → 同一 partition → 有序)
    ↓
Gateway (每个实例独立消费全部 partition):
    1. 检查 localRooms[X] 是否存在
    2. 存在 → Distributor 扇出 → 客户端
    3. 不存在 → 丢弃
```

**房间关闭（批量清理）：**

```
场景：直播结束，5万用户需要离开

原方案：5万次 LeaveRoom 消息
新方案：1 RoomClose × 5 Gateway = 5条消息

POST /api/v1/room/close {room_id}
    ↓
Roomserver 向各 Gateway 发送 RoomClose{room_id}
    ↓
Gateway 批量清理 localRooms[room_id]，通知客户端
```

### 5.3 背压与保护机制

#### 消息分级

```
┌──────────────────────────────────────────────────────────────────┐
│  队列状态      │  Critical  │  High    │  Normal  │  Low        │
├──────────────────────────────────────────────────────────────────┤
│  正常 (<50%)   │  ✓ 发送    │  ✓ 发送  │  ✓ 发送  │  ✓ 发送     │
│  预警 (50%-80%)│  ✓ 发送    │  ✓ 发送  │  ✓ 发送  │  ✗ 丢弃     │
│  严重 (80%-95%)│  ✓ 发送    │  ✓ 发送  │  ✗ 丢弃  │  ✗ 丢弃     │
│  危险 (>95%)   │  ✓ 发送    │  ✗ 丢弃  │  ✗ 丢弃  │  ✗ 丢弃     │
└──────────────────────────────────────────────────────────────────┘
```

#### 连接保护

```
断连条件（任一触发）：
  - 队列满持续 > 30s
  - 连续丢弃 > 100 次
  - 连续写超时 > 10 次（单次超时 500ms）
```

#### Distributor 分片架构

```
               Kafka 消息
                   ↓
              Dispatcher
                   ↓
    ┌──────────────┼──────────────┐
    ↓              ↓              ↓
Distributor-1  Distributor-2  Distributor-N
(inputQueue)   (inputQueue)   (inputQueue)
    ↓              ↓              ↓
  fanout (非阻塞投递到 conn.sendCh)
    ↓
conn.writeLoop (独立 goroutine，批量写)

设计要点：
  - Distributor：CPU 密集，不阻塞
  - writeLoop：IO 密集，独立隔离慢连接
  - select + default：队列满立即放弃，保护快连接
```

### 5.4 心跳弱自愈

```
Client ──Heartbeat{room_id}──> Gateway
                                  │
                   检查 localRooms 中用户的 room_id
                                  │
       ┌──────────┬───────────────┴────────────────┐
       ▼          ▼                                ▼
     一致     不一致（mismatch）               不存在（not_in_room）
   → 正常    → 返回错误码 + gateway_room_id    → 返回错误码

客户端处理：
  - mismatch: Leave(gateway_room_id) → Join(client_room_id)
  - not_in_room: Join(client_room_id)
```

### 5.5 消息可靠性保障（Seq + RingBuffer + 客户端补漏）

#### 问题背景

```
Python → Kafka → Gateway → Distributor → Connection → Client
                              ↓
                    inputQueue 满时消息丢弃
                              ↓
                    Python 无感知，无法重试
```

实时推送是 Fire-and-Forget，慢连接导致的消息丢失无法避免。

#### 解决方案：分层保障

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 1: 实时推送（Best-Effort）                            │
├─────────────────────────────────────────────────────────────┤
│  Python → Kafka → Gateway → Client                          │
│  - 每条消息带 seq（room 级别单调递增）                         │
│  - inputQueue 满就丢，接受这个事实                            │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│  Layer 2: 消息暂存（Gateway RingBuffer）                     │
├─────────────────────────────────────────────────────────────┤
│  Gateway 维护 per-room 环形缓冲区：                           │
│                                                             │
│  roomBuffers[room_id] = RingBuffer{                        │
│      capacity: 1000,  // 最近 1000 条                        │
│      messages: [{seq, payload, timestamp}, ...]             │
│  }                                                          │
│                                                             │
│  消息推送时同时写入 buffer（O(1) 操作，不阻塞）                 │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│  Layer 3: 客户端补漏（Seq Gap Detection）                    │
├─────────────────────────────────────────────────────────────┤
│  Client 维护 lastSeq[room_id]                               │
│                                                             │
│  收到消息时检测 gap：                                         │
│    if msg.seq > lastSeq + 1:                               │
│        send(PullRequest{room_id, from_seq: lastSeq + 1})   │
│                                                             │
│  Gateway 收到 PullRequest：                                  │
│    从 roomBuffers[room_id] 返回 seq 范围内的消息              │
└─────────────────────────────────────────────────────────────┘
```

#### 协议扩展

```
新增消息类型：
  MsgTypePullRequest   = 0x0020  // 客户端请求补漏
  MsgTypePullResponse  = 0x1020  // 服务端返回补漏消息

PullRequest 格式：
  { room_id, from_seq, to_seq }

PullResponse 格式：
  { room_id, messages: [{seq, payload}, ...] }
```

#### 边界情况

| 场景 | 处理方式 |
|-----|---------|
| 消息太老，buffer 中没有 | 返回 "gap_too_large"，客户端显示提示或从业务 API 拉取 |
| Gateway 重启，buffer 丢失 | 客户端重连 JoinRoom，seq 重置 |
| 网络恢复后大量补漏请求 | 限流：单连接每秒最多 1 次 PullRequest |

#### 资源开销

| 项目 | 估算 |
|-----|------|
| 内存 | 1000条/房间 × 1KB/条 × 1000房间 ≈ 1GB |
| CPU（写 buffer） | O(1)，可忽略 |
| 补漏请求占比 | < 1%（仅慢连接需要） |

---

## 六、故障恢复

### 6.1 Resume 流程（Client ↔ Gateway 断连）

```
触发：客户端网络抖动 或 Gateway 崩溃
影响：WebSocket 断开，Roomserver 标记 disconnected
客户端感知：有（需要主动 Resume）

流程：
  Client 重连（可能连到不同 Gateway）
      ↓
  发送 Resume{user_id, room_id}
      ↓
  Roomserver 检查：
    - disconnected → 恢复，更新 gateway_id
    - 超时被清除 → 410，要求重新 JoinRoom
    - room_id 不匹配 → 409，返回 current_room_id
    - 房间已关闭 → 410 Room Closed

第一版不做消息重放（直播场景消息时效性强）
```

### 6.2 BatchReSync（Roomserver 崩溃恢复）

```
触发：Roomserver 进程崩溃/重启
影响：内存数据丢失，Client ↔ Gateway 不受影响
客户端感知：无（Gateway 处理）

流程：
  Roomserver 启动，epoch=当前时间戳，进入 RECOVERY MODE
      ↓
  广播 EpochNotify 给所有 Gateway
      ↓
  Gateway 检测 epoch 变化，渐进式上报 BatchReSync
    - 每批 1000 用户，间隔 10ms
    - 只上报 CONFIRMED 状态
      ↓
  Roomserver 合并状态（最新 Gateway 为准）
      ↓
  退出条件：超时(30s) 或 所有 Gateway 完成上报
      ↓
  进入 SERVING，处理缓冲请求
```

### 6.3 Gateway 与 Roomserver 网络分区

```
核心原则：
  - 踢用户没有意义（连不上 Roomserver，重连也无法 JoinRoom）
  - 数据面（Kafka）独立，消息推送不受影响
  - 保证数据面可用，控制面优雅降级

┌─────────────────────────────────────────────────────────────────┐
│  阶段 1：短期断连（< 30s）                                         │
├─────────────────────────────────────────────────────────────────┤
│  Gateway 状态：NORMAL                                            │
│  控制面请求返回 503，客户端退避重试                                 │
│  数据面正常，已在房间用户继续收消息                                 │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│  阶段 2：长期断连（>= 30s）                                        │
├─────────────────────────────────────────────────────────────────┤
│  Gateway 状态：DEGRADED                                          │
│  /health 返回 degraded（ELB 不分配新连接）                         │
│  数据面继续正常                                                   │
│  控制面降级：                                                     │
│    - JoinRoom：返回 503                                          │
│    - LeaveRoom：本地执行，记录 pendingLeaves 待补偿                │
│    - Resume：检查 localRooms，有则成功，无则 503                   │
│  触发告警，运维介入                                                │
└─────────────────────────────────────────────────────────────────┘

恢复后：BatchReSync + 处理 pendingLeaves + 恢复 NORMAL
```

---

## 七、部署架构

### 7.1 实例规划（100K 在线）

| 组件 | 实例数 | 单实例负载 |
|------|--------|-----------|
| Gateway | 2 | 20K-35K 连接 |
| Roomserver | 2 | 一致性哈希分担房间 |
| Nacos | 3（集群） | 服务注册/配置 |

### 7.2 Roomserver 分片与高可用设计

#### 设计决策

| 决策项 | 结论 | 理由 |
|-------|------|------|
| 分片策略 | 按 room_id 一致性哈希分片 | 水平扩展清晰 |
| 主从复制 | **不做** | 复杂度高，收益有限 |
| 单房间跨分片 | **不做** | 复杂度远超收益 |
| 崩溃恢复 | BatchReSync 从 Gateway 重建 | Gateway 持有活连接，是权威数据源 |
| Snapshot | 定时写入，仅用于灾难恢复 | 运维兜底，不用于自动恢复 |

#### 恢复流程

**正常恢复（单 Roomserver 分片崩溃）**:
```
Roomserver 重启
  → Gateway 检测断连（最长 30s）
  → Gateway 发送 BatchReSync（渐进式，1000 用户/批次）
  → Roomserver 重建状态
  → 恢复服务

预期恢复时间：30s - 90s
影响范围：仅该分片负责的房间
```

**灾难恢复（Gateway + Roomserver 同时挂）**:
```
运维从 Snapshot 手动恢复房间元数据
  → 用户重新连接 JoinRoom
```

#### Snapshot 实现要点
- 定时写入（每 30s）
- 存储内容：房间元数据、用户列表快照
- 存储位置：本地文件或 Redis
- 异步写入，不阻塞主流程

### 7.3 客户端接入方案（Dispatcher + 直连）

#### 设计决策

| 方案 | 描述 | 优缺点 |
|------|------|--------|
| ELB 全量转发 | 所有 WebSocket 流量经 ELB | 简单但延迟高、成本高、ELB 成瓶颈 |
| **Dispatcher + 直连** | 首次获取地址，后续直连 | ✅ 推荐：延迟低、成本低、可扩展 |

#### 连接流程

```
┌────────┐   1. GET /api/gateway   ┌────────────┐
│ Client │ ──────────────────────→ │ Dispatcher │  (可放在 ELB 后)
└────────┘                         └────────────┘
    │                                    │
    │                              2. 返回最优 Gateway
    │                                 {"addr": "wss://gw3.im.example.com"}
    │                                    ↓
    │         3. WebSocket 直连（不经 ELB）
    └────────────────────────────────────────────→ Gateway-3
                                                   (后续流量直连)
```

#### Dispatcher API

```
GET /api/gateway/recommend?user_id=xxx

Response:
{
  "gateway": {
    "addr": "wss://gw3.im.example.com:443",
    "load": 0.6
  },
  "fallback": [
    "wss://gw1.im.example.com:443",
    "wss://gw2.im.example.com:443"
  ]
}
```

#### Dispatcher 选择策略

```go
func selectGateway(clientIP string) *Gateway {
    gateways := getHealthyGateways()           // 1. 获取健康节点
    sort.Slice(gateways, func(i, j) bool {     // 2. 按负载排序
        return gateways[i].LoadRatio() < gateways[j].LoadRatio()
    })
    return gateways[0]                         // 3. 返回最优节点
}
```

#### 客户端重连策略

```
1. WebSocket 断开
2. 立即重试原 Gateway（可能只是临时抖动）
3. 失败 3 次后，调用 Dispatcher 获取新地址
4. 连接新 Gateway，发送 Resume 请求
5. 指数退避：1s → 2s → 4s → 8s → max 30s（±20% jitter）
```

#### 部署要点

- Dispatcher 可部署在 ELB 后面（HTTP API，流量小）
- Gateway 需要独立公网 IP 或域名（直连需要）
- Gateway 健康状态定期上报到 Dispatcher


---

## 八、技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.21+ |
| 传输层 | gorilla/websocket |
| 服务间通信 | gRPC（stream）+ msgpack |
| 消息队列 | Kafka |
| 序列化 | msgpack（内部）、JSON（外部） |
| 服务发现 | Nacos |
| 一致性哈希 | hashicorp/consistent |
| 监控 | Prometheus + Grafana |
| 日志 | zap |
| 认证 | JWT (RS256) |

### Kafka 配置

| 配置项 | 值 |
|--------|-----|
| 推送 Topic | im_msg_broadcast |
| 业务请求 Topic | im_biz_request |
| Consumer Group | gateway_id（独立消费全部消息）|

### Nacos 配置

| 配置项 | 值 |
|--------|-----|
| Gateway 服务名 | gateway |
| Roomserver 服务名 | roomserver |
| 刷新间隔 | 5s |
| JWT 公钥 | jwt.public_key |

---

## 九、配置参数汇总

### 时间参数

| 参数 | 值 | 说明 |
|------|-----|------|
| RECOVERY MODE 超时 | 30s | Roomserver 恢复最长等待 |
| Disconnected 超时 | 60s | 断线后保留状态时间 |
| 心跳间隔 | 20s | 客户端建议值 |
| DEGRADED 触发阈值 | 30s | 连不上 Roomserver |
| 用户断连通知超时 | 3s | 单次，重试 2 次 |
| RoomClose 单 Gateway 超时 | 3s | 重试 2 次，总上限 10s |

### 队列/容量参数

| 参数 | 值 |
|------|-----|
| BatchReSync 批次 | 1000 用户，间隔 10ms |
| Distributor 分片数 | CPU 核数 × 2 |
| Conn SendCh | 1024 |
| 批量发送 | 间隔 10ms，上限 10 条 |

### 连接保护参数

| 参数 | 值 |
|------|-----|
| 队列预警/严重/危险 | 50%/80%/95% |
| 队列满超时 | 30s |
| 连续丢弃阈值 | 100 |
| 写超时 | 500ms，连续 10 次断开 |

---

## 十、第一版范围

**实现：**
- Gateway + Roomserver 核心架构
- WebSocket 连接管理、心跳、断线检测
- JoinRoom / LeaveRoom / Resume
- 房间广播、单播、多播
- ACK 驱动状态更新
- 消息分级、背压、连接保护
- 心跳弱自愈
- Roomserver 崩溃恢复（BatchReSync）
- 基本监控指标（Prometheus）

**延后：**
- 消息压缩（带宽瓶颈时）
- 扩缩容逻辑（用户增长时）
- 消息重放（有明确需求时）
- QUIC 传输（移动网络优化需求时）
- Kafka 消费优化/Dispatcher 层（CPU 占比超 30%）
- 大房间拆分（需要 50K 用户/房间时）

