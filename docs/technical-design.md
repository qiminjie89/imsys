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
    gatewayID string
    ip        string
    port      int

    // 连接管理
    connections  map[string]*Connection   // user_id → connection
    distributors []*Distributor           // 按 hash(user_id) 分配连接

    // 本地状态（用于消息过滤和扇出）
    localRooms map[string]map[string]bool // room_id → Set<user_id>
    localUsers map[string]bool            // user_id → true

    // 消息补漏缓存
    roomBuffers map[string]*RingBuffer    // room_id → RingBuffer

    // Roomserver 连接
    roomserverStreams map[string]*grpc.ClientStream
    knownEpochs       map[string]uint64
    hashRing          *ConsistentHash

    // Kafka
    kafkaConsumer *kafka.Consumer  // consumer_group = gateway_id

    // 状态
    status        GatewayStatus    // NORMAL / DEGRADED
    pendingLeaves []PendingRequest
}

type Connection struct {
    UserID      string
    WSConn      *websocket.Conn
    criticalCh  chan []byte       // Critical 消息队列（容量小，优先级高）
    normalCh    chan []byte       // 普通消息队列（容量大）
    JoinState   JoinState
    RoomID      string
    distributor *Distributor
    health      ConnHealth
}

type RingBuffer struct {
    capacity int                  // 1000
    messages []BufferedMessage    // 环形数组
    head     int
    tail     int
    mu       sync.RWMutex
}

type BufferedMessage struct {
    Seq       uint64
    Payload   []byte
    Timestamp time.Time
}
```

#### 消息推送完整流程

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Gateway 消息推送流程                                │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  1. Kafka Consumer (单 goroutine)                                    │   │
│  │                                                                      │   │
│  │  for msg := range consumer.Messages() {                             │   │
│  │      // 快速过滤：只读 Header，不反序列化 Payload                      │   │
│  │      roomID := msg.Headers.Get("room_id")                           │   │
│  │      if !localRooms.Has(roomID) {                                   │   │
│  │          continue  // 本 Gateway 无该房间用户，跳过                    │   │
│  │      }                                                              │   │
│  │      handleMessage(msg)                                             │   │
│  │  }                                                                  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  2. handleMessage (消息处理)                                         │   │
│  │                                                                      │   │
│  │  func handleMessage(msg) {                                          │   │
│  │      // Step 1: 完整反序列化                                          │   │
│  │      payload := msgpack.Unmarshal(msg.Value)                        │   │
│  │                                                                      │   │
│  │      // Step 2: 先写 RingBuffer（保证可补漏）                          │   │
│  │      roomBuffers[roomID].Write(payload.Seq, msg.Value)              │   │
│  │                                                                      │   │
│  │      // Step 3: 获取房间内用户列表                                     │   │
│  │      users := localRooms[roomID]                                    │   │
│  │                                                                      │   │
│  │      // Step 4: 按用户分发到对应 Distributor                          │   │
│  │      for userID := range users {                                    │   │
│  │          dist := distributors[hash(userID) % N]                     │   │
│  │          dist.inputQueue <- {userID, payload}  // 非阻塞             │   │
│  │      }                                                              │   │
│  │  }                                                                  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                    ┌────────────────┼────────────────┐                     │
│                    ▼                ▼                ▼                     │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  3. Distributor (N 个 goroutine，N = CPU × 2)                        │   │
│  │                                                                      │   │
│  │  ┌─────────────┐   ┌─────────────┐   ┌─────────────┐                │   │
│  │  │Distributor-1│   │Distributor-2│   │Distributor-N│                │   │
│  │  │ inputQueue  │   │ inputQueue  │   │ inputQueue  │                │   │
│  │  └──────┬──────┘   └──────┬──────┘   └──────┬──────┘                │   │
│  │         │                 │                 │                        │   │
│  │         ▼                 ▼                 ▼                        │   │
│  │  for item := range inputQueue {                                     │   │
│  │      conn := connections[item.userID]                               │   │
│  │      if item.priority == "critical" {                               │   │
│  │          // Critical: 带超时的阻塞写入                                 │   │
│  │          select {                                                   │   │
│  │          case conn.criticalCh <- item.payload:                      │   │
│  │          case <-time.After(100ms): conn.markUnhealthy()             │   │
│  │          }                                                          │   │
│  │      } else {                                                       │   │
│  │          // Normal: 非阻塞写入                                        │   │
│  │          select {                                                   │   │
│  │          case conn.normalCh <- item.payload:                        │   │
│  │          default: conn.dropCount++                                  │   │
│  │          }                                                          │   │
│  │      }                                                              │   │
│  │  }                                                                  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                    ┌────────────────┼────────────────┐                     │
│                    ▼                ▼                ▼                     │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  4. Connection writeLoop (每连接 1 个 goroutine)                      │   │
│  │                                                                      │   │
│  │  ┌──────────┐    ┌──────────┐    ┌──────────┐                       │   │
│  │  │ Conn-1   │    │ Conn-2   │    │ Conn-M   │                       │   │
│  │  │criticalCh│    │criticalCh│    │criticalCh│   (容量: 64)          │   │
│  │  │ normalCh │    │ normalCh │    │ normalCh │   (容量: 1024)        │   │
│  │  └────┬─────┘    └────┬─────┘    └────┬─────┘                       │   │
│  │       │               │               │                              │   │
│  │       ▼               ▼               ▼                              │   │
│  │  writeLoop:                                                          │   │
│  │    // 优先消费 criticalCh                                             │   │
│  │    select {                                                          │   │
│  │    case msg := <-criticalCh: writeWithRetry(msg)                    │   │
│  │    default:                                                          │   │
│  │    }                                                                │   │
│  │    // 再消费 normalCh                                                 │   │
│  │    select {                                                          │   │
│  │    case msg := <-criticalCh: writeWithRetry(msg)                    │   │
│  │    case msg := <-normalCh:   batchAndWrite(msg)                     │   │
│  │    case <-ticker.C:          flushBatch()                           │   │
│  │    }                                                                │   │
│  │       │               │               │                              │   │
│  │       ▼               ▼               ▼                              │   │
│  │  WebSocket-1     WebSocket-2     WebSocket-M                        │   │
│  │   (阻塞写)        (阻塞写)        (阻塞写)                             │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

#### 消息补漏流程 (PullRequest)

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  客户端检测到 Seq Gap                                                        │
│                                                                             │
│  收到消息 seq=105，但 lastSeq=100                                            │
│       │                                                                     │
│       ▼                                                                     │
│  发送 PullRequest{room_id, from_seq:101, to_seq:104}                        │
│       │                                                                     │
│       ▼                                                                     │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  Gateway 处理 PullRequest                                            │   │
│  │                                                                      │   │
│  │  func handlePullRequest(req) {                                      │   │
│  │      buffer := roomBuffers[req.RoomID]                              │   │
│  │      messages := buffer.GetRange(req.FromSeq, req.ToSeq)            │   │
│  │                                                                      │   │
│  │      if len(messages) < expected {                                  │   │
│  │          // 消息太旧，已被覆盖                                         │   │
│  │          return PullResponse{status: "gap_too_large"}               │   │
│  │      }                                                              │   │
│  │                                                                      │   │
│  │      return PullResponse{messages: messages}                        │   │
│  │  }                                                                  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│       │                                                                     │
│       ▼                                                                     │
│  客户端收到 PullResponse，填补 seq 101-104 的消息                             │
│  更新 lastSeq = 105                                                         │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

#### 关键设计要点

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  设计要点                              │  说明                              │
├─────────────────────────────────────────────────────────────────────────────┤
│  1. Header 过滤在前                    │  减少 90% 无效反序列化              │
│  2. RingBuffer 写入在 Fanout 之前       │  保证丢弃的消息仍可补漏              │
│  3. Distributor 非阻塞写 sendCh        │  慢连接不阻塞其他连接                │
│  4. Critical 双队列 + 优先消费          │  重要消息优先触达                   │
│  5. writeLoop 批量写入                 │  减少 WebSocket 写次数              │
│  6. 每连接独立 goroutine               │  慢连接隔离                         │
└─────────────────────────────────────────────────────────────────────────────┘
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

```
// Topic: im_msg_broadcast
// 压缩: lz4（生产端配置）

Headers:
  scope: "room"              // 用于快速过滤，无需反序列化 payload
  room_id: "xxx"             // scope=room 时
  user_id: "yyy"             // scope=user 时
  priority: "normal"         // critical/high/normal/low

Payload (msgpack):
{
    "scope": "room",
    "room_id": "xxx",
    "user_id": "yyy",          // scope=user 时
    "user_ids": ["a", "b"],    // scope=multicast 时
    "seq": 12345,
    "priority": "normal",
    "message": { ... }
}

// Partition Key: room_id（房间广播）或 user_id（单播）
// 每个 Gateway 独立 consumer_group，消费全部 partition，本地过滤
```

### 4.5 Kafka 消费优化

#### 问题背景

```
每个 Gateway 独立消费全部 Kafka 分区，大部分消息被过滤：
  - 10 个 Gateway，每条消息被消费 10 次
  - 单 Gateway 可能只有 10% 的房间用户
  - 90% 的消息反序列化后被丢弃 → CPU 浪费
```

#### 优化方案

**1. Kafka Header + 延迟反序列化**

```go
// Python 发送时：路由信息放到 Kafka Header
producer.send(
    topic="im_msg_broadcast",
    key=room_id,
    headers={
        "scope": scope,
        "room_id": room_id,    // 用于快速过滤
        "priority": priority,
    },
    value=msgpack.dumps(payload),
)

// Gateway 消费时：先检查 Header，匹配后才反序列化
func (g *Gateway) consumeMessage(msg *kafka.Message) {
    // Step 1: 快速过滤（只读 header，不反序列化 payload）
    scope := msg.Headers.Get("scope")
    roomID := msg.Headers.Get("room_id")

    if scope == "room" && !g.localRooms.Has(roomID) {
        return  // 快速跳过，CPU 开销极小
    }

    // Step 2: 匹配后才反序列化完整 payload
    var payload PushPayload
    msgpack.Unmarshal(msg.Value, &payload)
    g.distribute(roomID, payload)
}
```

**2. Kafka lz4 压缩**

```yaml
# Kafka Producer 配置
compression.type: lz4    # 压缩比 2-3x，解压速度 4GB/s

# 效果：
#   - 网络带宽下降 60%
#   - CPU 开销可忽略（lz4 解压极快）
```

#### 效果评估

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 反序列化次数 | 100% | 10%（仅匹配消息） |
| CPU 用于过滤 | 高 | 低（Header 解析成本 ~5%） |
| 网络带宽 | 100% | 40%（lz4 压缩） |

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

#### Distributor 参数配置

```go
const (
    DistributorCount      = runtime.NumCPU() * 2  // 分片数
    DistributorQueueSize  = 10000                 // inputQueue 容量
    ConnCriticalChSize    = 64                    // Critical 队列容量
    ConnNormalChSize      = 1024                  // Normal 队列容量
    RingBufferCapacity    = 1000                  // 每房间缓存消息数
    BatchInterval         = 10 * time.Millisecond // 批量发送间隔
    BatchMaxSize          = 10                    // 单批最大消息数
)
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

**BatchReSyncAck 主动通知**：

当 BatchReSyncAck 返回 `rejected` 或 `corrected` 的用户时，Gateway 主动推送通知给受影响的客户端，而不是等待下次心跳：

```
BatchReSyncAck 返回
    │
    ├── rejected 用户列表
    │       │
    │       └── 向客户端推送 RoomMismatch{状态: not_in_room}
    │
    └── corrected 用户列表
            │
            └── 向客户端推送 RoomMismatch{状态: wrong_room, correct_room_id}
```

客户端收到通知后，按心跳弱自愈的逻辑处理。

### 5.5 消息可靠性保障（Seq + RingBuffer + 客户端补漏）

#### 设计原理

```
实时推送是 Fire-and-Forget，慢连接导致的消息丢失无法避免。
解决方案：消息先存 RingBuffer，再 Fanout，客户端通过 Seq 检测 Gap 并补漏。
```

#### 三层保障

| 层次 | 机制 | 说明 |
|------|------|------|
| Layer 1 | 实时推送 | Best-Effort，sendCh 满则丢弃 |
| Layer 2 | RingBuffer | 消息先存后推，保证可补漏 |
| Layer 3 | 客户端补漏 | 检测 Seq Gap，发送 PullRequest |

#### 关键实现顺序

```go
func (g *Gateway) handleMessage(msg *Message) {
    // ⚠️ 顺序很重要：先存后推

    // Step 1: 写入 RingBuffer（保证丢弃的消息仍可补漏）
    g.roomBuffers[msg.RoomID].Write(msg.Seq, msg.Payload)

    // Step 2: Fanout 到连接（可能丢弃）
    for userID := range g.localRooms[msg.RoomID] {
        dist := g.getDistributor(userID)
        select {
        case dist.inputQueue <- msg:
        default:
            // 丢弃，但 RingBuffer 里还有
        }
    }
}
```

#### RingBuffer 设计

```go
// 环形缓冲区，O(1) 写入，自动覆盖旧消息
type RingBuffer struct {
    capacity int               // 1000 条/房间
    messages []BufferedMessage
    seqIndex map[uint64]int    // seq → 数组下标（快速查找）
}

// 写入：永不阻塞
func (r *RingBuffer) Write(seq uint64, payload []byte) {
    idx := r.tail % r.capacity
    r.messages[idx] = BufferedMessage{Seq: seq, Payload: payload}
    r.seqIndex[seq] = idx
    r.tail++

    // 清理被覆盖的旧 seq
    if r.tail > r.capacity {
        oldSeq := r.messages[(r.tail-r.capacity-1) % r.capacity].Seq
        delete(r.seqIndex, oldSeq)
    }
}

// 读取：用于 PullRequest
func (r *RingBuffer) GetRange(fromSeq, toSeq uint64) []BufferedMessage {
    // 返回 [fromSeq, toSeq] 范围内的消息
}
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

#### Epoch 机制

```
作用：标识 Roomserver 启动周期，用于检测 Gateway 是否错过重启

Roomserver 重启时：
  epoch = time.Now().UnixMilli()  // 新时间戳

Gateway 连接时：
  发送 GatewayInfo{knownEpoch}
  Roomserver 比较 epoch，不同则请求 BatchReSync
```

#### 恢复流程

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  场景 1：Roomserver 正常重启                                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  Roomserver 重启                                                            │
│      │                                                                      │
│      ├── epoch = 当前时间戳                                                  │
│      ├── 进入 RECOVERY MODE                                                 │
│      │                                                                      │
│      ▼                                                                      │
│  向所有已连接 Gateway 发送 BatchReSyncRequest{epoch}                         │
│      │                                                                      │
│      ▼                                                                      │
│  Gateway 收到请求，主动发起 BatchReSync（阻塞 RPC）                           │
│      │                                                                      │
│      ├── 分批上报：每批 1000 用户，间隔 10ms                                  │
│      ├── 携带信息：userID, roomID, gatewayID, lastActiveAt                  │
│      │                                                                      │
│      ▼                                                                      │
│  Roomserver 处理数据                                                        │
│      │                                                                      │
│      ├── 冲突解决：同一用户多 Gateway → 以 lastActiveAt 最新为准              │
│      ├── 使用 Roomserver 收到时间，避免时钟偏差                               │
│      │                                                                      │
│      ▼                                                                      │
│  返回 BatchReSyncAck{confirmed, rejected, corrected}                        │
│      │                                                                      │
│      ├── confirmed：确认一致                                                │
│      ├── rejected：用户不在该 Gateway（Gateway 清除本地）                    │
│      ├── corrected：用户在不同房间（Gateway 更新本地）                        │
│      │                                                                      │
│      ▼                                                                      │
│  Gateway 根据 ACK 刷新本地缓存                                               │
│      │                                                                      │
│      ▼                                                                      │
│  所有 Gateway 完成（或超时 30s）→ 退出 RECOVERY MODE → SERVING               │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│  场景 2：Gateway 因分区错过 BatchReSyncRequest                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  分区期间 Roomserver 重启，epoch 变更                                        │
│      │                                                                      │
│      ▼                                                                      │
│  分区恢复，Gateway 重新连接                                                  │
│      │                                                                      │
│      ├── Gateway 发送 GatewayInfo{knownEpoch=旧值}                          │
│      │                                                                      │
│      ▼                                                                      │
│  Roomserver 检测 epoch 不匹配                                               │
│      │                                                                      │
│      ├── 发送 BatchReSyncRequest{epoch=新值}                                │
│      │                                                                      │
│      ▼                                                                      │
│  Gateway 发起 BatchReSync（同场景 1）                                        │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

#### pendingLeaves 过期处理

```go
type PendingLeave struct {
    UserID    string
    RoomID    string
    CreatedAt time.Time
    ExpireAt  time.Time  // TTL = 5 分钟
}

// 分区恢复后处理
func (g *Gateway) processPendingLeaves() {
    now := time.Now()
    for _, pl := range g.pendingLeaves {
        if now.After(pl.ExpireAt) {
            continue  // 过期丢弃
        }
        g.sendLeaveRequest(pl)  // Roomserver 会校验用户是否仍在该 Gateway
    }
    g.pendingLeaves = nil
}
```

### 6.3 Gateway 与 Roomserver 网络分区

```
核心原则：
  - 数据面（Kafka）独立，消息推送不受影响
  - 控制面优雅降级，分区恢复后自动修复
  - Roomserver 是权威数据源
```

#### 分区处理流程

```
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
│  /health 返回 degraded（Dispatcher 不分配新连接）                  │
│  数据面继续正常                                                   │
│  控制面降级：                                                     │
│    - JoinRoom：返回 503                                          │
│    - LeaveRoom：本地执行，记录 pendingLeaves（TTL 5分钟）          │
│    - Resume：检查 localRooms，有则成功，无则 503                   │
│  触发告警，运维介入                                                │
└─────────────────────────────────────────────────────────────────┘
```

#### 分区恢复流程

```
分区恢复
    │
    ├── Gateway 重新连接 Roomserver
    │
    ├── 发送 GatewayInfo{knownEpoch}
    │       │
    │       ├── epoch 相同 → 无需 BatchReSync
    │       │       │
    │       │       └── 处理 pendingLeaves
    │       │
    │       └── epoch 不同 → Roomserver 请求 BatchReSync
    │               │
    │               └── Gateway 发起 BatchReSync
    │                       │
    │                       └── 根据 ACK 刷新本地数据
    │
    └── Gateway 恢复 NORMAL 状态
```

#### 用户操作场景

```
场景 A：用户无操作
  → 消息收发正常（数据面独立）

场景 B：用户 Leave 房间
  → Leave 失败 → 本地 pendingLeave → 返回客户端成功
  → 分区恢复 → 发送 Leave → Roomserver 处理

场景 C：用户 Leave 后 Join 其他房间
  → Join 失败（503）→ 重试 3 次
  → 客户端获取新 Gateway（如 Gateway2）
  → 断开 Gateway1（Gateway1 清除本地用户数据）
  → 连接 Gateway2 → Join 成功

场景 D：场景 C 后分区恢复
  → Gateway1 处理 pendingLeave
  → Roomserver 发现用户已在 Gateway2 → 忽略
```

#### Gateway 断连处理

```
当 Gateway 与 Roomserver 断连时，Roomserver 不立即清除该 Gateway 上的用户状态：

Roomserver 检测到 Gateway 断连
    │
    └── 启动 5 分钟延迟计时器
            │
            ├── 5 分钟内 Gateway 重连 → 取消计时器，继续服务
            │
            └── 5 分钟后 Gateway 仍未重连
                    │
                    └── 标记该 Gateway 上所有用户为 disconnected
                        （触发 60s 用户断线超时计时器）
```

**设计理由**：
- 短暂网络抖动不应触发用户状态变更
- 5 分钟足够覆盖大多数网络恢复场景
- 与 pendingLeaves TTL（5 分钟）保持一致
- 真正长期断连的情况，由 BatchReSync 机制处理

```go
func (r *Roomserver) onGatewayDisconnect(gatewayID string) {
    time.AfterFunc(5*time.Minute, func() {
        if r.isGatewayConnected(gatewayID) {
            return  // 已重连，无需处理
        }
        users := r.getUsersByGateway(gatewayID)
        for _, user := range users {
            r.markDisconnected(user.UserID)
        }
    })
}
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
┌─────────────────────────────────────────────────────────────────────────┐
│  场景 1：Gateway 断连                                                    │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  WebSocket 断开                                                         │
│      │                                                                  │
│      ├── 立即重连当前 Gateway（可能只是短暂抖动）                          │
│      │                                                                  │
│      ├── 连接成功 → 发送 Resume                                          │
│      │       │                                                          │
│      │       ├── 成功 → 恢复正常                                         │
│      │       │                                                          │
│      │       ├── 410 (用户已过期/房间已关闭) → 直接 JoinRoom              │
│      │       │                                                          │
│      │       ├── 409 (room_id 不匹配) → 按返回的 current_room_id 处理     │
│      │       │                                                          │
│      │       └── 503 (服务不可用) → 请求 Dispatcher 换 Gateway            │
│      │                                                                  │
│      └── 连接失败 → jitter 后重试，失败 3 次后请求 Dispatcher 换 Gateway   │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│  场景 2：JoinRoom 持续失败                                               │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  JoinRoom 失败 × 3                                                      │
│      │                                                                  │
│      └── 请求 Dispatcher 获取可用 Gateway 列表                           │
│              │                                                          │
│              ├── 列表非空 → 断开当前 Gateway，连接新 Gateway              │
│              │                                                          │
│              └── 列表为空（所有 Gateway 都 DEGRADED）                     │
│                      │                                                  │
│                      └── 指数退避后重试当前 Gateway                       │
│                                                                         │
│  说明：Gateway 与 Roomserver 网络分区时会向 Dispatcher 汇报 DEGRADED，    │
│        Dispatcher 不会返回不可用的 Gateway，自然形成熔断                   │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│  退避策略                                                                │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  基础间隔：1s                                                            │
│  指数增长：1s → 2s → 4s → 8s → max 30s                                   │
│  Jitter：±20%                                                           │
│                                                                         │
│  全局熔断（Dispatcher 返回空列表时）：                                     │
│    30s → 60s → 120s → max 5min                                          │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│  Dispatcher 列表刷新时机                                                 │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  1. JoinRoom/Resume 失败需要换 Gateway 时                                │
│  2. 缓存 TTL（5 分钟）过期后                                              │
│  3. 熔断恢复后                                                           │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
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

| 参数 | 值 | 说明 |
|------|-----|------|
| Distributor 分片数 | CPU × 2 | 并行 fanout |
| Distributor inputQueue | 10000 | 消息入队缓冲 |
| Conn criticalCh | 64 | Critical 消息队列 |
| Conn normalCh | 1024 | 普通消息队列 |
| RingBuffer 容量 | 1000 条/房间 | 消息补漏缓存 |
| 批量发送间隔 | 10ms | writeLoop 批量写 |
| 批量发送上限 | 10 条 | 单批最大消息数 |
| BatchReSync 批次 | 1000 用户，间隔 10ms | Roomserver 恢复 |

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
- **Distributor 分片 + 双队列（Critical/Normal）**
- **RingBuffer + 客户端 PullRequest 补漏**
- 消息分级、背压、连接保护
- 心跳弱自愈
- Roomserver 崩溃恢复（BatchReSync）
- 基本监控指标（Prometheus）
- **Kafka Header + 延迟反序列化**（消费优化）
- **Kafka lz4 压缩**（带宽优化）

**延后：**
- 扩缩容逻辑（用户增长时）
- 消息重放（有明确需求时）
- QUIC 传输（移动网络优化需求时）
- Dispatcher 层（Gateway > 50 且 Kafka CPU > 30% 时）
- 大房间拆分（需要 50K 用户/房间时）

