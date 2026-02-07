# IMSys 技术设计方案

> 面向 **直播房间 / 大规模在线 / 高并发广播** 场景的 IM 架构设计

---

## 一、项目背景

现有直播玩法系统（抽奖、礼物等）基于 Python/Tornado 提供 REST API，客户端（Android/iOS）与后端之间缺乏异步通信能力。本项目搭建一套 Go 语言实现的 IM 系统，打通后端与客户端的异步消息通道。

### 1.1 容量目标

**第一版目标：**
- 单房间最大用户数：10,000
- 系统同时在线用户数：100,000
- 单机房部署

**远期目标（待优化）：**
- 单房间最大用户数：50,000
- 单房间 QPS：500
- 详见 architecture-discussion.md 第十节瓶颈分析

### 1.2 设计目标

- 支撑 **十万级在线连接**、**万级大房间广播**
- 数据面高可用，控制面可快速恢复
- 不依赖强一致存储，追求 **工程可控性与可演进性**
- 能优雅应对：节点重启、扩缩容、网络抖动

### 1.3 关键约束

```
1. 用户同时只能在一个房间（user → room 是 1:1 关系）
2. Join/Leave 是用户行为，Resume 处理断线重连（用户无感知）
3. Gateway 副本仅用于广播扇出（Roomserver 广播时不传用户列表）
4. 纯内存设计，不使用 Redis 缓存（Join/Leave 高频）
5. 可接受短暂不一致，目标是系统可持续服务
```

---

## 二、核心设计思想

### 2.1 控制面 vs 数据面

```
┌─────────────────────────────────────────────────────────────────┐
│  数据面（Data Plane）                                            │
│    - Client ↔ Gateway 的长连接                                  │
│    - 消息下发、心跳、sendQueue、背压                              │
│    - 要求：不中断、低延迟                                        │
├─────────────────────────────────────────────────────────────────┤
│  控制面（Control Plane）                                         │
│    - 用户进出房间（Join / Leave）                                │
│    - user ↔ room、room ↔ gateway 映射                          │
│    - 允许短暂中断、可重建                                        │
└─────────────────────────────────────────────────────────────────┘

设计原则：控制面允许失忆，数据面必须坚挺
```

### 2.2 一致性取舍

```
不做：
  ✗ Roomserver 主从复制
  ✗ 状态双写对账
  ✗ 强一致复制

选择的原则：
  ✓ 可恢复 > 完全正确
  ✓ 简单 > 复杂
  ✓ 可演进 > 理论完美
```

---

## 三、系统架构

### 3.1 整体架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                        客户端 (Android/iOS)                       │
└────────────────────────────┬────────────────────────────────────┘
                             │ WebSocket + TLS
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                     ELB (华为云负载均衡)                           │
│                     健康检查：GET /health                         │
└────────────────────────────┬────────────────────────────────────┘
                             │ WebSocket
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Gateway (接入层，多实例)                       │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌───────────────────┐  │
│  │ 连接管理  │ │ 协议编解码│ │ 消息路由  │ │ 本地房间缓存(扇出) │  │
│  └──────────┘ └──────────┘ └──────────┘ └───────────────────┘  │
└──────────┬───────────────────────────────────┬─────────────────┘
           │ gRPC Stream (双向)                 │ Kafka (业务请求)
           ▼                                    │
┌──────────────────────────────────┐            │
│      Roomserver (房间服务)        │            │
│  ┌───────────┐ ┌───────────┐    │            │
│  │RoomService│ │DispatchSvc│    │            │
│  │SessionSvc │ │(消息分发)  │    │            │
│  └───────────┘ └─────┬─────┘    │            │
└──────────────────────┼──────────┘            │
                       │ HTTP + JSON            │
                       │ (推送接口)              │
                       ▼                        ▼
                    ┌─────────────────────────────┐
                    │   Python 玩法后端 (已有)      │
                    │   (抽奖、礼物等玩法服务)       │
                    └─────────────────────────────┘

服务发现：Nacos（注册/发现） + gRPC 连接级检测（实时故障感知）
```

### 3.2 请求流向

| 场景 | 链路 |
|------|------|
| 客户端业务请求（抽奖、送礼） | Client → Gateway → Kafka → Python 玩法后端 |
| 房间操作（JoinRoom/LeaveRoom） | Client → Gateway → Roomserver |
| 恢复连接（Resume） | Client → Gateway → Roomserver |
| 后端推送消息给客户端 | Python 后端 → Roomserver → Gateway → Client |
| 房间广播 | Roomserver → 多个 Gateway → 各自本地扇出 |

---

## 四、组件详细设计

### 4.1 Gateway（接入层）

#### 核心职责

- 客户端 WebSocket 连接管理（建连、心跳、断线检测）
- 轻量鉴权（JWT 本地验证，仅首次连接校验）
- 协议编解码（消息封包/拆包、压缩）
- 消息路由分发：
  - 客户端异步请求（业务消息）→ Kafka → Python 后端
  - 房间操作（JoinRoom/LeaveRoom/Resume）→ Roomserver
- 本地房间成员缓存（仅用于广播扇出）
- sendQueue + 背压控制
- 向 Nacos 注册自身服务信息
- 健康检查接口（供 ELB 探测）

#### 负载均衡架构

```
Client ↔ ELB (华为云) ↔ Gateway-1
                      ↔ Gateway-2
                      ↔ Gateway-N

ELB 健康检查：
  - 协议：HTTP
  - 路径：GET /health
  - 间隔：5s
  - 超时：3s
  - 健康阈值：2 次成功
  - 不健康阈值：3 次失败
```

#### 健康检查接口

```
GET /health

响应示例（健康）：
HTTP 200 OK
{
    "status": "healthy",
    "connections": 12500,
    "roomserver_connected": true,
    "kafka_connected": true,
    "uptime_seconds": 3600
}

响应示例（不健康）：
HTTP 503 Service Unavailable
{
    "status": "unhealthy",
    "reason": "roomserver_disconnected",
    "connections": 12500,
    "roomserver_connected": false,
    "kafka_connected": true
}
```

```go
// 健康检查实现
func (g *Gateway) HealthHandler(w http.ResponseWriter, r *http.Request) {
    health := &HealthStatus{
        Connections:         len(g.connections),
        RoomserverConnected: g.isRoomserverConnected(),
        KafkaConnected:      g.kafkaProducer.IsConnected(),
        UptimeSeconds:       time.Since(g.startTime).Seconds(),
    }

    if health.RoomserverConnected && health.KafkaConnected {
        health.Status = "healthy"
        w.WriteHeader(http.StatusOK)
    } else {
        health.Status = "unhealthy"
        if !health.RoomserverConnected {
            health.Reason = "roomserver_disconnected"
        } else {
            health.Reason = "kafka_disconnected"
        }
        w.WriteHeader(http.StatusServiceUnavailable)
    }

    json.NewEncoder(w).Encode(health)
}
```

#### 数据结构

```go
type Gateway struct {
    // 连接管理
    connections map[string]*Connection  // user_id → connection

    // Distributor 分片（广播扇出）
    distributors []*Distributor  // 按 hash(user_id) 分配连接

    // 本地房间成员缓存（仅用于广播扇出，ACK 驱动更新）
    localRooms map[string]map[string]bool  // room_id → Set<user_id>

    // 与 Roomserver 的 gRPC stream
    roomserverStreams map[string]*grpc.ClientStream  // roomserver_id → stream

    // 已知的 Roomserver epoch（用于检测重启）
    knownEpoch uint64

    // Kafka 生产者（业务请求转发）
    kafkaProducer *kafka.Producer
}

type Distributor struct {
    id         int
    inputQueue chan []byte           // 压缩后的消息
    conns      map[string]*Connection // 该分片的连接
    mu         sync.RWMutex
}

type Connection struct {
    UserID      string
    ConnID      string
    WSConn      *websocket.Conn
    sendCh      chan []byte       // 下行消息队列（压缩后）
    dropCount   int               // 连续丢弃次数（慢连接检测）
    JoinState   JoinState         // JoinRoom 状态机
    AuthTime    time.Time
    distributor *Distributor      // 所属分片
}
```

#### JoinRoom 状态机

```
状态流转：INIT → PENDING → CONFIRMED

┌──────────────────────────────────────────────────────────────┐
│  INIT                                                        │
│    - 用户未加入任何房间                                        │
│    - 不参与广播                                               │
├──────────────────────────────────────────────────────────────┤
│  PENDING                                                     │
│    - JoinRoom 请求已发给 Roomserver，等待 ACK                  │
│    - 不参与广播                                               │
│    - 可安全丢弃（Roomserver 崩溃时）                           │
├──────────────────────────────────────────────────────────────┤
│  CONFIRMED                                                   │
│    - 收到 Roomserver ACK                                     │
│    - 参与广播                                                │
│    - BatchReSync 只上报 CONFIRMED 状态                        │
└──────────────────────────────────────────────────────────────┘

关键规则：
  - Gateway 必须收到 Roomserver ACK 后才更新 localRooms
  - 只有 CONFIRMED 状态才参与广播
  - BatchReSync 只上报 CONFIRMED 状态
```

#### 鉴权流程

```
客户端建立 WebSocket 连接
    ↓
发送首个消息携带 token（JWT）
    ↓
Gateway 本地验证 JWT（签名校验 + 过期时间检查）
    ↓ 验证通过
标记连接为已认证，绑定 user_id
    ↓
后续该连接上的请求不再校验 token
    ↓
Token 过期 → Gateway 断开连接，客户端重新获取 token 后重连
```

#### 传输层抽象（预留 QUIC 迁移）

```go
// 传输层接口，后续可切换为 QUIC 实现
type Transport interface {
    Listen(addr string) error
    Accept() (Connection, error)
    Close() error
}

type TransportConn interface {
    Read() ([]byte, error)
    Write(data []byte) error
    Close() error
    RemoteAddr() string
}

// 当前实现
type WebSocketTransport struct { ... }

// 未来实现
// type QUICTransport struct { ... }
```

---

### 4.2 Roomserver（房间服务）

#### 核心职责

- 房间管理（用户进出房间、房间成员维护）
- 会话管理（用户与 Gateway 的映射关系）
- 消息分发（单播、房间广播、全局广播）
- 暴露推送接口供 Python 后端调用
- 内部转发（路由过时时，转发请求到正确节点）
- 控制面裁决权威

#### 特性

```
- 单权威数据源
- 纯内存态存储
- 可随时重启
- 不恢复历史，只重建当前可服务状态
```

#### 水平扩展：Dynamo 风格一致性哈希

```
对 room_id 做一致性哈希 → 同一个房间的所有操作路由到同一个 Roomserver 实例

优势：
- 内存状态 + 一致性哈希 = 水平可扩展
- 无需 Redis 存储房间关系
- 单房间单实例处理，天然保序
```

#### 数据结构

```go
type RoomServer struct {
    status    ServerStatus  // RECOVERING / READY

    // 房间数据
    rooms map[string]*Room  // room_id → Room

    // 用户会话（1:1 关系，用户只能在一个房间）
    users map[string]*UserState  // user_id → state

    // Gateway 连接管理
    gateways map[string]*GatewayConn  // gateway_id → connection

    // Roomserver 集群（用于内部转发）
    hashRing  *ConsistentHash                    // 一致性哈希环
    peers     map[string]*grpc.ClientConn        // peer_id → gRPC 连接
}

type Room struct {
    RoomID   string
    Seq      uint64  // 消息序列号（单调递增，goroutine 串行处理无需锁）

    // Room Lane：每房间一个 goroutine
    msgCh    chan *BroadcastRequest  // 广播请求队列
    done     chan struct{}           // 用于关闭 goroutine

    // 房间成员（由 Room goroutine 串行访问，无需锁）
    Members  map[string]*RoomMember  // user_id → member

    // 房间内 Gateway 列表（用于广播路由）
    Gateways map[string]*GatewayRoomView  // gateway_id → 该 Gateway 上的用户列表
}

type RoomMember struct {
    UserID         string
    GatewayID      string    // 当前连接的 Gateway
    Status         MemberStatus  // Online / Disconnected
    JoinTime       time.Time
    DisconnectTime time.Time
}

type GatewayRoomView struct {
    GatewayID string
    UserIDs   map[string]bool  // 该 Gateway 上属于此房间的用户集合
}

type UserState struct {
    UserID    string
    RoomID    string    // 当前所在房间（只有一个）
    GatewayID string
    Status    MemberStatus
}

type ServerStatus int
const (
    StatusRecovering ServerStatus = iota
    StatusReady
)

type MemberStatus int
const (
    StatusOnline MemberStatus = iota
    StatusDisconnected
)
```

#### 用户状态模型

```
                    JoinRoom
  not_in_room ─────────────────> online
                                   │
                              连接断开 │ LeaveRoom
                                   │       │
                                   ▼       ▼
                             disconnected  not_in_room
                                   │
                       Resume(重连) │ 超时(30-60s)
                                   │       │
                                   ▼       ▼
                               online   not_in_room
```

#### 核心接口

```go
// 供 Gateway 调用（通过 gRPC stream）
type RoomServerService interface {
    JoinRoom(userID, roomID, gatewayID string) error
    LeaveRoom(userID, roomID string) error
    Resume(userID, roomID, gatewayID string) error
    BatchReSync(gatewayID string, users []UserRoomPair) error  // 崩溃恢复
}

// 供 Python 玩法后端调用（HTTP + JSON）
type PushService interface {
    Unicast(userID string, msg Message) error
    Multicast(roomID string, userIDs []string, msg Message) error  // 多播：发给指定用户列表
    RoomBroadcast(roomID string, msg Message) error
    GlobalBroadcast(msg Message) error
}
```

---

### 4.3 基础服务模块

#### 配置管理

- 使用 Nacos 配置中心，支持动态配置更新
- 本地配置文件作为 fallback

#### 日志模块

- 结构化日志（JSON 格式）
- 分级输出（Debug/Info/Warn/Error）
- 关键路径日志：连接建立/断开、JoinRoom/LeaveRoom、消息推送

#### 监控模块

- 关键指标：连接数、房间数、消息 QPS、推送延迟、错误率
- Prometheus metrics 暴露
- 关键告警：连接数突增/突降、推送延迟过高、Roomserver 不可用

---

## 五、通信协议设计

### 5.1 客户端 ↔ Gateway（WebSocket）

#### 消息帧格式

```
+----------+----------+----------+------------------+
|  MsgType |   Seq    |  Length  |     Payload      |
|  4 bytes |  8 bytes |  4 bytes |   变长 (msgpack)  |
+----------+----------+----------+------------------+
```

#### 消息类型定义

```go
const (
    // 客户端 → Gateway
    MsgTypeAuth       = 0x0001  // 认证请求
    MsgTypeJoinRoom   = 0x0002  // 加入房间
    MsgTypeLeaveRoom  = 0x0003  // 离开房间
    MsgTypeResume     = 0x0004  // 断线重连恢复
    MsgTypeBizRequest = 0x0010  // 业务请求（转发给 Python 后端）
    MsgTypeHeartbeat  = 0x00FF  // 心跳（可带 room_id 用于弱自愈）

    // Gateway → 客户端
    MsgTypeAuthResp      = 0x1001  // 认证响应
    MsgTypeJoinRoomResp  = 0x1002  // 加入房间响应
    MsgTypeLeaveRoomResp = 0x1003  // 离开房间响应
    MsgTypeResumeResp    = 0x1004  // Resume 响应
    MsgTypePushMessage   = 0x1010  // 推送消息
    MsgTypeBizResponse   = 0x1011  // 业务响应
    MsgTypeHeartbeatResp = 0x10FF  // 心跳响应
)
```

### 5.2 Gateway ↔ Roomserver（gRPC 双向 Stream）

#### Proto 定义

```protobuf
syntax = "proto3";

package imsys;

message Envelope {
    uint32 msg_type = 1;   // 消息类型编号
    bytes  payload  = 2;   // msgpack 序列化的业务数据
    uint64 seq      = 3;   // 序列号（用于请求-响应匹配）
}

service RoomServerGateway {
    // 双向流：Gateway 和 Roomserver 之间的长连接通道
    rpc Channel(stream Envelope) returns (stream Envelope);
}
```

#### 内部消息类型

```go
const (
    // Gateway → Roomserver
    InternalMsgJoinRoom       = 0x2001
    InternalMsgLeaveRoom      = 0x2002
    InternalMsgResume         = 0x2003
    InternalMsgBatchReSync    = 0x2004  // 崩溃恢复批量同步
    InternalMsgGatewayInfo    = 0x2005  // Gateway 上报自身信息
    InternalMsgBatchLeave     = 0x2006  // 批量离开（恢复后处理断开的连接）
    InternalMsgReSyncComplete = 0x2007  // BatchReSync 完成标记
    InternalMsgDisconnect     = 0x2008  // 用户断连通知（异步）

    // Roomserver → Gateway
    InternalMsgJoinRoomAck   = 0x3001  // JoinRoom/LeaveRoom/Resume ACK
    InternalMsgBroadcast     = 0x3002  // 广播消息（只带 room_id + seq）
    InternalMsgUnicast       = 0x3003  // 单播消息
    InternalMsgEpochNotify   = 0x3004  // Epoch 变更通知（触发 Gateway 重新上报）
    InternalMsgRoomClose     = 0x3005  // 房间关闭通知（批量清理，避免消息风暴）
    InternalMsgMulticast     = 0x3006  // 多播消息（带用户列表）
)
```

### 5.3 Gateway / Python 玩法后端 通信

#### 5.3.1 业务请求（Gateway → Kafka → Python）

```
客户端发送业务请求（抽奖、送礼等）：
  Client → Gateway → Kafka → Python 后端

Kafka 消息格式：
{
    "msg_id": "unique_id",
    "user_id": "xxx",
    "room_id": "xxx",
    "type": "lottery_draw",  // 业务类型：lottery_draw, send_gift, etc.
    "data": { ... },
    "timestamp": 1706745600000
}

特点：
  - 异步，不阻塞 Gateway
  - Gateway 只负责转发，不关心业务逻辑
  - Python 后端按需处理，结果通过推送接口返回
  - 解耦 Go IM 系统与 Python 业务系统
```

#### 5.3.2 推送接口（Python → Roomserver，HTTP + JSON）

**路由策略**

```
Python 后端路由到 Roomserver：

1. 订阅 Nacos 获取 Roomserver 节点列表
2. 本地维护一致性哈希环
3. 发送请求时：hash(room_id) → 选择目标节点
4. 兜底机制：如果路由过时，Roomserver 内部转发

┌─────────────────────────────────────────────────────────────────┐
│  正常情况（99%+）                                                │
│    Python ──hash(room_id)──> Roomserver-2（直达）               │
├─────────────────────────────────────────────────────────────────┤
│  节点变更期间（Nacos 延迟）                                       │
│    Python ──hash(room_id)──> Roomserver-1（路由过时）            │
│                                   │                             │
│                              内部转发                            │
│                                   ▼                             │
│                              Roomserver-2（正确处理）            │
└─────────────────────────────────────────────────────────────────┘
```

**接口定义**

```
POST /api/v1/push/unicast
{
    "room_id": "xxx",    // 必须，用于路由
    "user_id": "yyy",
    "message": {
        "type": "lottery_result",
        "data": { ... }
    }
}

POST /api/v1/push/multicast
{
    "room_id": "xxx",      // 必须，用于路由
    "user_ids": ["a", "b", "c"],  // 目标用户列表
    "message": {
        "type": "private_gift_notify",
        "data": { ... }
    }
}

POST /api/v1/push/room_broadcast
{
    "room_id": "xxx",
    "message": {
        "type": "gift_animation",
        "data": { ... }
    }
}

POST /api/v1/push/global_broadcast
{
    "message": { ... }
}

POST /api/v1/room/close
{
    "room_id": "xxx",
    "reason": "ended"  // ended, banned, etc.
}
```

**Roomserver 内部转发**

```
Roomserver 收到推送请求：
  1. 计算 hash(room_id)，判断是否归属自己
  2. 是自己 → 直接处理
  3. 不是 → 转发给正确节点（gRPC）→ 返回结果

Roomserver 之间通信：复用 gRPC 连接
```

---

## 六、核心机制设计

### 6.1 Gateway 本地状态管理（ACK 驱动模型）

#### 设计原则

```
核心思想：
  - Gateway 的 localRooms 只包含连接到本 Gateway 的用户
  - 不需要知道其他 Gateway 上的用户
  - 状态更新由 ACK 驱动，因果链清晰
```

#### 状态更新流程

**JoinRoom（ACK 驱动）：**

```
Client ──JoinRoom──> Gateway-1 ──JoinRoom──> Roomserver
                                                  │
                                             处理成功
                                                  │
Client <───ACK───── Gateway-1 <─────ACK───────────┘
                         │
                   收到 ACK 后：
                   1. 状态 PENDING → CONFIRMED
                   2. localRooms[room].add(user)
                   3. 返回 ACK 给 Client

关键：只有收到 ACK 才更新 localRooms
```

**LeaveRoom（ACK 驱动）：**

```
Client ──LeaveRoom──> Gateway-1 ──LeaveRoom──> Roomserver
                                                    │
                                               处理成功
                                                    │
Client <───ACK────── Gateway-1 <─────ACK────────────┘
                          │
                    收到 ACK 后：
                    1. localRooms[room].remove(user)
                    2. 返回 ACK 给 Client
```

**断连（立即本地更新）：**

```
WebSocket 断开
      │
      ▼
Gateway-1: localRooms[room].remove(user)  ← 立即移除，无需等 ACK
      │
      ▼
Gateway-1 ──通知断连──> Roomserver（异步）
```

**Resume（ACK 驱动）：**

```
Client 重连到 Gateway-2
      │
      ▼
Gateway-2 ──Resume──> Roomserver
                           │
                      更新 gateway_id
                           │
Gateway-2 <────ACK─────────┘
      │
      ▼
localRooms[room].add(user)
```

#### 消息定义

```go
// 崩溃恢复批量同步（保留）
type BatchReSync struct {
    GatewayID string          `msgpack:"gateway_id"`
    Users     []UserRoomPair  `msgpack:"users"`
}

type UserRoomPair struct {
    UserID string `msgpack:"user_id"`
    RoomID string `msgpack:"room_id"`
}
```

---

### 6.2 广播消息流程

#### 房间广播（Roomserver 不传用户列表）

```
Python 后端:
POST /api/v1/push/room_broadcast {room_id=X, message=M}
    ↓
Roomserver:
    1. 请求进入 room 的 msgCh（channel）
    2. Room goroutine 串行处理：seq++（无需锁）
    3. 查 room.Gateways → [gateway-1, gateway-2, gateway-3]
    4. 向每个 Gateway 发送: Broadcast{room_id=X, seq=N, message=M}
       （不携带用户列表，节省带宽）
    ↓
Gateway 收到 Broadcast:
    1. 查 localRooms[X] → [user_1, user_2, ..., user_2000]
    2. Distributor 本地扇出：遍历用户推送消息
```

#### 单播

```
Python 后端:
POST /api/v1/push/unicast {user_id=A, message=M}
    ↓
Roomserver:
    1. 查 users[A].GatewayID → gateway-2
    2. 向 gateway-2 发送: Unicast{user_id=A, message=M}
    ↓
Gateway-2:
    1. 查 connections[A] → conn
    2. conn.SendQueue <- M
```

#### 多播（Multicast）

```
场景：发送消息给房间内指定的一组用户
  - 中奖通知（只发给中奖者）
  - 特定用户组的私密通知
  - 部分用户的个性化推送

Python 后端:
POST /api/v1/push/multicast {room_id=X, user_ids=[A,B,C], message=M}
    ↓
Roomserver:
    1. 遍历 user_ids
    2. 按 gateway_id 分组：
       - gateway-1: [A, C]
       - gateway-2: [B]
    3. 向每个 Gateway 发送: Multicast{room_id=X, user_ids=[...], message=M}
    ↓
Gateway 收到 Multicast:
    1. 遍历 user_ids
    2. 查 connections[user] → conn
    3. conn.SendQueue <- M

优势（相比逐个 Unicast）：
  - 减少 HTTP 请求数（1 次 vs N 次）
  - Roomserver 批量处理，减少锁竞争
  - Gateway 接收 1 条消息 vs N 条消息
```

**Multicast 消息定义：**

```go
type Multicast struct {
    RoomID  string   `msgpack:"room_id"`
    UserIDs []string `msgpack:"user_ids"`
    Message []byte   `msgpack:"message"`  // 压缩后的消息体
}
```

#### 房间关闭（批量清理，避免消息风暴）

```
场景：直播结束，房间关闭，5万用户需要离开

问题：如果逐个发送 LeaveRoom，会产生消息风暴
  - 5万次 LeaveRoom 消息
  - 5万次状态更新
  - 系统压力巨大

解决方案：RoomClose 批量通知

Python 后端:
POST /api/v1/room/close {room_id=X}
    ↓
Roomserver:
    1. 查 room.Gateways → [gateway-1, gateway-2, gateway-3]
    2. 向每个 Gateway 发送: RoomClose{room_id=X}
    3. 清理 Roomserver 本地状态：
       - 删除 rooms[X]
       - 批量更新 users 状态
    ↓
Gateway 收到 RoomClose:
    1. 批量清理 localRooms[X]（整个房间删除）
    2. 通知房间内所有客户端（可选，带 reason）
    3. 清理连接的房间状态
```

**RoomClose 消息定义：**

```go
type RoomClose struct {
    RoomID string `msgpack:"room_id"`
    Reason string `msgpack:"reason"`  // 关闭原因：ended, banned, etc.
}
```

**Gateway 处理 RoomClose：**

```go
func (g *Gateway) onRoomClose(msg RoomClose) {
    // 1. 获取该房间的所有本地用户
    users := g.localRooms[msg.RoomID]

    // 2. 批量通知客户端（可选）
    for userID := range users {
        if conn, ok := g.connections[userID]; ok {
            conn.sendRoomClosedNotify(msg.RoomID, msg.Reason)
            conn.JoinState = StateInit  // 重置状态
        }
    }

    // 3. 批量清理本地状态
    delete(g.localRooms, msg.RoomID)
}
```

**效果对比：**

```
原方案（逐个 LeaveRoom）：
  5万用户 × 1 LeaveRoom = 5万条消息

新方案（RoomClose）：
  1 RoomClose × 5 Gateway = 5条消息

消息量降低：10000 倍
```

---

### 6.3 消息保序设计

#### 整体保序链路

```
消息源（Python后端）
    ↓ HTTP 请求到达顺序 = 消息最终顺序
Roomserver（每房间 1 个 goroutine，串行分配 seq）  ← 唯一串行化点
    ↓ gRPC stream（TCP，有序）
Gateway（Distributor 扇出，不负责保序）
    ↓ WebSocket（TCP，有序）
客户端（按 room_id + seq 排序/去重）
```

#### Room Lane（无锁 seq 分配）

```
设计目标：
  - 高并发下避免锁竞争
  - 同一房间消息严格有序
  - 利用 goroutine 天然串行属性

核心思想：
  - 每个 Room = 1 个 goroutine = 1 个 Lane
  - 消息顺序由业务方调用广播接口的先后顺序决定
  - goroutine 内部按接收顺序递增 seq，无需加锁

┌─────────────────────────────────────────────────────────────────────────┐
│                     RoomServer                                          │
│                                                                         │
│   Business Service A ──┐      ┌─────────────────────────────────────┐  │
│   Business Service B ──┼──────│  Room 123 (goroutine)               │  │
│   Business Service C ──┘      │                                     │  │
│                               │  msgCh ──> seq++ ──> broadcast      │  │
│                               │  (channel)  (串行)   (to gateways)  │  │
│                               │                                     │  │
│   HTTP 请求到达顺序            │  seq: 1, 2, 3, 4, 5...              │  │
│   = 消息最终顺序               └─────────────────────────────────────┘  │
│                                                                         │
│   ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐     │
│   │ Room 456 (goro)  │  │ Room 789 (goro)  │  │ Room N (goro)    │     │
│   │ seq: 100         │  │ seq: 50          │  │ seq: 200         │     │
│   └──────────────────┘  └──────────────────┘  └──────────────────┘     │
└─────────────────────────────────────────────────────────────────────────┘

消息格式：
  {
      "room_id": "room_123",
      "seq": 101,
      "message": { ... }
  }

客户端处理：
  - 按 room_id 分组
  - 同一房间按 seq 排序/去重
```

**Room Lane 实现：**

```go
type RoomServer struct {
    rooms map[string]*Room  // room_id → Room
    mu    sync.RWMutex      // 保护 rooms map
}

type Room struct {
    RoomID   string
    seq      uint64
    msgCh    chan *BroadcastRequest  // 广播请求队列
    gateways map[string]*GatewayConn // 该房间涉及的 Gateway
    done     chan struct{}           // 用于关闭 goroutine
}

// 创建房间时启动 goroutine
func (rs *RoomServer) getOrCreateRoom(roomID string) *Room {
    rs.mu.Lock()
    defer rs.mu.Unlock()

    if room, ok := rs.rooms[roomID]; ok {
        return room
    }

    room := &Room{
        RoomID:   roomID,
        msgCh:    make(chan *BroadcastRequest, 1000),
        gateways: make(map[string]*GatewayConn),
        done:     make(chan struct{}),
    }
    rs.rooms[roomID] = room

    // 启动该房间的处理 goroutine
    go room.run()
    return room
}

// Room goroutine：串行处理，天然有序
func (r *Room) run() {
    for {
        select {
        case req := <-r.msgCh:
            r.seq++
            req.Seq = r.seq
            r.broadcastToGateways(req)
        case <-r.done:
            return
        }
    }
}

// 广播入口
func (r *Room) Broadcast(msg Message) {
    r.msgCh <- &BroadcastRequest{
        RoomID:  r.RoomID,
        Message: msg,
    }
}

// 房间关闭时清理
func (r *Room) Close() {
    close(r.done)
}
```

**性能分析：**

```
单 goroutine 处理能力：
  - seq++ 和写 channel：~1μs
  - 理论吞吐：100万 QPS
  - 500 QPS 场景：完全没有瓶颈

背压处理：
  - msgCh 有界（如 1000）
  - 队列满时：返回 503 或短暂阻塞
  - 上游业务可感知并降速
```

---

### 6.4 背压与保护机制

#### 各层背压职责

| 层 | 背压关注点 | 策略 |
|----|-----------|------|
| WebSocket 传输层 | TCP 发送缓冲区满 | 由 OS/TCP 协议栈处理 |
| Gateway 应用层 | 下行消息队列积压 | 消息分级 + 多维度保护 |
| Gateway → Roomserver | gRPC stream 写入慢 | 应用层熔断、请求排队 |
| Roomserver → 上游 | 广播处理能力不足 | 返回 503，上游限流 |

#### 消息分级

```go
type MessagePriority int

const (
    PriorityCritical MessagePriority = 0  // 关键消息：系统通知、踢人、房间关闭
    PriorityHigh     MessagePriority = 1  // 高优消息：礼物特效、中奖通知
    PriorityNormal   MessagePriority = 2  // 普通消息：弹幕、聊天
    PriorityLow      MessagePriority = 3  // 低优消息：在线人数更新、点赞动画
)

// 消息结构
type Message struct {
    Priority  MessagePriority
    Timestamp time.Time
    Payload   []byte
}
```

**分级策略：**

```
┌─────────────────────────────────────────────────────────────────────────┐
│  队列状态          │  Critical  │  High    │  Normal  │  Low           │
├─────────────────────────────────────────────────────────────────────────┤
│  正常 (<50%)       │  ✓ 发送    │  ✓ 发送  │  ✓ 发送  │  ✓ 发送        │
│  预警 (50%-80%)    │  ✓ 发送    │  ✓ 发送  │  ✓ 发送  │  ✗ 丢弃        │
│  严重 (80%-95%)    │  ✓ 发送    │  ✓ 发送  │  ✗ 丢弃  │  ✗ 丢弃        │
│  危险 (>95%)       │  ✓ 发送    │  ✗ 丢弃  │  ✗ 丢弃  │  ✗ 丢弃        │
└─────────────────────────────────────────────────────────────────────────┘
```

#### 连接健康度模型

```go
type ConnHealth struct {
    // 队列维度
    QueueUsage        float64       // 队列使用率 = len(sendCh) / cap(sendCh)
    QueueFullDuration time.Duration // 队列满持续时长
    QueueFullSince    time.Time     // 队列开始满的时间

    // 投递维度
    DropCount         int           // Distributor 投递丢弃次数（累计）
    ConsecutiveDrops  int           // 连续丢弃次数

    // 写入维度
    WriteTimeoutCount int           // 写超时次数（累计）
    ConsecutiveWriteTimeouts int    // 连续写超时次数
    LastWriteTime     time.Time     // 最后成功写入时间
}
```

#### 多维度保护策略

```
┌─────────────────────────────────────────────────────────────────────────┐
│  保护维度              │  预警阈值        │  危险阈值        │  动作       │
├─────────────────────────────────────────────────────────────────────────┤
│  队列使用率            │  80%            │  95%            │  降级/断开   │
│  队列满持续时长         │  5s             │  30s            │  断开       │
│  连续投递丢弃次数       │  50             │  100            │  断开       │
│  连续写超时次数         │  3              │  10             │  断开       │
│  写超时阈值            │  500ms          │  -              │  计入超时   │
└─────────────────────────────────────────────────────────────────────────┘
```

**保护实现：**

```go
const (
    // 队列阈值
    QueueWarningRatio   = 0.5   // 50% - 开始丢弃 Low
    QueueSevereRatio    = 0.8   // 80% - 开始丢弃 Normal
    QueueCriticalRatio  = 0.95  // 95% - 开始丢弃 High

    // 断连阈值
    QueueFullTimeout    = 30 * time.Second  // 队列满超过 30s 断开
    MaxConsecutiveDrops = 100               // 连续丢弃 100 次断开
    MaxConsecutiveWriteTimeouts = 10        // 连续写超时 10 次断开
    WriteTimeout        = 500 * time.Millisecond  // 单次写超时阈值
)

// Distributor 投递逻辑（带分级）
func (d *Distributor) dispatch(msg Message) {
    for _, conn := range d.conns {
        queueRatio := float64(len(conn.sendCh)) / float64(cap(conn.sendCh))

        // 根据队列状态和消息优先级决定是否投递
        shouldDrop := false
        switch {
        case queueRatio >= QueueCriticalRatio:
            shouldDrop = msg.Priority > PriorityCritical
        case queueRatio >= QueueSevereRatio:
            shouldDrop = msg.Priority > PriorityHigh
        case queueRatio >= QueueWarningRatio:
            shouldDrop = msg.Priority > PriorityNormal
        }

        if shouldDrop {
            conn.health.DropCount++
            conn.health.ConsecutiveDrops++
            d.checkAndCloseSlowConn(conn)
            continue
        }

        // 非阻塞投递
        select {
        case conn.sendCh <- msg.Payload:
            conn.health.ConsecutiveDrops = 0  // 成功则重置连续计数
            conn.health.QueueFullSince = time.Time{}
        default:
            conn.health.DropCount++
            conn.health.ConsecutiveDrops++
            if conn.health.QueueFullSince.IsZero() {
                conn.health.QueueFullSince = time.Now()
            }
            d.checkAndCloseSlowConn(conn)
        }
    }
}

// 检查并关闭慢连接
func (d *Distributor) checkAndCloseSlowConn(conn *Connection) {
    h := &conn.health

    // 连续丢弃次数过多
    if h.ConsecutiveDrops >= MaxConsecutiveDrops {
        conn.closeWithReason("consecutive_drops_exceeded")
        return
    }

    // 队列满时间过长
    if !h.QueueFullSince.IsZero() {
        if time.Since(h.QueueFullSince) >= QueueFullTimeout {
            conn.closeWithReason("queue_full_timeout")
            return
        }
    }
}

// Conn 写循环（带超时检测）
func (c *Conn) writeLoop() {
    ticker := time.NewTicker(BatchInterval)
    batch := make([][]byte, 0, BatchMaxSize)

    for {
        select {
        case msg, ok := <-c.sendCh:
            if !ok {
                c.flushBatch(batch)
                return
            }
            batch = append(batch, msg)
            if len(batch) >= BatchMaxSize {
                if !c.flushBatchWithTimeout(batch) {
                    return
                }
                batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                if !c.flushBatchWithTimeout(batch) {
                    return
                }
                batch = batch[:0]
            }
        }
    }
}

// 带超时的批量写
func (c *Conn) flushBatchWithTimeout(batch [][]byte) bool {
    if len(batch) == 0 {
        return true
    }

    c.ws.SetWriteDeadline(time.Now().Add(WriteTimeout))
    packed := packMessages(batch)
    err := c.ws.WriteMessage(websocket.BinaryMessage, packed)

    if err != nil {
        if isTimeout(err) {
            c.health.WriteTimeoutCount++
            c.health.ConsecutiveWriteTimeouts++
            if c.health.ConsecutiveWriteTimeouts >= MaxConsecutiveWriteTimeouts {
                c.closeWithReason("write_timeout_exceeded")
                return false
            }
        } else {
            c.closeWithReason("write_error")
            return false
        }
    } else {
        c.health.ConsecutiveWriteTimeouts = 0
        c.health.LastWriteTime = time.Now()
    }
    return true
}
```

#### 监控指标

```go
// 需要暴露的 Prometheus 指标
var (
    connQueueUsage = prometheus.NewHistogram(...)       // 队列使用率分布
    connDropTotal  = prometheus.NewCounter(...)         // 总丢弃消息数
    connCloseReason = prometheus.NewCounterVec(...)     // 断连原因分布
    connWriteTimeout = prometheus.NewCounter(...)       // 写超时次数
    msgDropByPriority = prometheus.NewCounterVec(...)   // 按优先级统计丢弃
)
```

---

## 七、故障恢复

### 7.1 场景区分

```
┌─────────────────────────────────────────────────────────────────────┐
│  场景 1：Client ↔ Gateway 断连（Resume 流程）                        │
├─────────────────────────────────────────────────────────────────────┤
│  触发条件：                                                          │
│    - 客户端网络抖动                                                  │
│    - Gateway 崩溃/重启                                               │
│                                                                     │
│  影响：                                                              │
│    - 客户端 WebSocket 断开                                          │
│    - Roomserver 感知到用户断连，标记 disconnected                    │
│                                                                     │
│  恢复流程：                                                          │
│    - 客户端重连（可能连到新 Gateway）                                 │
│    - 发送 Resume 请求                                                │
│    - Roomserver 恢复用户状态                                         │
│                                                                     │
│  客户端感知：有感知（需要主动 Resume）                                │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  场景 2：Roomserver 崩溃（BatchReSync 流程）                         │
├─────────────────────────────────────────────────────────────────────┤
│  触发条件：                                                          │
│    - Roomserver 进程崩溃/重启                                        │
│                                                                     │
│  影响：                                                              │
│    - Roomserver 内存数据丢失                                         │
│    - Client ↔ Gateway 连接不受影响                                   │
│    - 用户仍在房间（Gateway localRooms 未变）                          │
│                                                                     │
│  恢复流程：                                                          │
│    - Gateway 检测到 gRPC 重连                                        │
│    - Gateway 发送 BatchReSync（基于 localRooms）                     │
│    - Roomserver 重建内存状态                                         │
│                                                                     │
│  客户端感知：无感知（Gateway 处理，对客户端透明）                      │
└─────────────────────────────────────────────────────────────────────┘
```

#### Resume vs JoinRoom vs BatchReSync

| 操作 | 语义 | 触发场景 | 客户端感知 |
|------|------|----------|-----------|
| JoinRoom | 用户进入直播间（业务行为） | 用户主动进入 | 有 |
| LeaveRoom | 用户退出直播间（业务行为） | 用户主动退出 | 有 |
| Resume | 恢复连接（客户端断连后） | 网络抖动/Gateway崩溃 | 有 |
| BatchReSync | 数据重建（Roomserver崩溃后） | Roomserver重启 | 无 |

#### Resume 流程（Client ↔ Gateway 断连后）

```
触发场景：客户端网络抖动 或 Gateway 崩溃

客户端检测到 WebSocket 断开
    ↓
自动重连（可能连到不同的 Gateway）
    ↓
发送 Resume{user_id, room_id}
    ↓
Gateway-2 转发给 Roomserver
    ↓
Roomserver:
    1. 检查用户状态是否为 disconnected
    2. 若已超时被清除 → 返回 404/410，要求重新 JoinRoom
    3. 更新 user.GatewayID = Gateway-2, status = Online
    4. 更新 room.Gateways 映射
    5. 给 Gateway-2 发送 ACK
    ↓
客户端恢复正常接收消息
```

#### Resume 失败处理

```
Resume 失败的错误码：

503 Service Unavailable
  - Roomserver 正在恢复（RECOVERING 状态）
  - 客户端：退避重试（1s, 2s, 4s...）

404 User Not Found
  - Roomserver 没有该用户记录
  - 客户端：fallback 到 JoinRoom

410 Gone
  - 用户已超时被清除
  - 客户端：fallback 到 JoinRoom
```

---

### 7.2 设计原则

```
┌─────────────────────────────────────────────────────────────┐
│  Roomserver                                                 │
│    - 单权威数据源                                            │
│    - 纯内存态                                                │
│    - 崩溃后数据可丢弃，从 Gateway 重建                        │
├─────────────────────────────────────────────────────────────┤
│  Gateway                                                    │
│    - 维护 localRooms 副本                                   │
│    - 仅用于广播扇出                                          │
│    - 只上报 CONFIRMED 状态                                  │
├─────────────────────────────────────────────────────────────┤
│  不做                                                       │
│    ✗ 双写对账                                               │
│    ✗ 主从复制                                               │
│    ✗ 状态持久化                                             │
└─────────────────────────────────────────────────────────────┘
```

### 7.3 Gateway 本地副本与 Roomserver 数据一致性三层防线

```
问题场景：
  用户 A JoinRoom，Roomserver 处理完毕
  Roomserver 正要返回 ACK 时崩溃
  Gateway 认为 A 不在房间（PENDING 状态被丢弃）
  重启后 Roomserver 从 Gateway 本地副本数据重建，但gateway数据是陈旧的

解决方案：三层防线

┌─────────────────────────────────────────────────────────────┐
│  第一层：ACK 顺序保证                                        │
├─────────────────────────────────────────────────────────────┤
│  Gateway 必须收到 Roomserver ACK 后才：                      │
│    1. 状态 PENDING → CONFIRMED                              │
│    2. 更新 localRooms                                       │
│    3. 返回 ACK 给 Client                                    │
│                                                             │
│  关键：Roomserver 崩溃前没返回 ACK → Gateway 不更新          │
│       PENDING 状态不上报 → 重建时数据准确                    │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│  第二层：幂等 + 重试                                         │
├─────────────────────────────────────────────────────────────┤
│  JoinRoom 设计：                                            │
│    - 幂等：用户已在房间时，再次 JoinRoom 返回成功            │
│    - 可重试：ACK 丢失时安全重试                              │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│  第三层：心跳弱自愈（第一版实现）                              │
├─────────────────────────────────────────────────────────────┤
│  Client ──Heartbeat{room_id}──> Gateway                     │
│                                    │                        │
│                    检查 localRooms 中用户的 room_id           │
│                                    │                        │
│       ┌──────────────┬─────────────┴──────────────────┐     │
│       ▼              ▼                                ▼     │
│    一致          不一致（客户端和Gateway记录不同）    不存在   │
│  → 正常         → 先 LeaveRoom 旧房间              → 补发 Join│
│                 → 再 JoinRoom 新房间               → 恢复一致│
│                 → 恢复一致                                   │
│                                                             │
│  实现细节：                                                 │
│    1. 客户端心跳消息携带当前 room_id                         │
│    2. Gateway 收到心跳后检查 localRooms                      │
│    3. 场景 A：用户不在任何房间但客户端声称在 room_id           │
│       → Gateway 代替用户发送 JoinRoom 给 Roomserver          │
│    4. 场景 B：用户在 room_A 但客户端声称在 room_B（不一致）    │
│       → Gateway 先发送 LeaveRoom(room_A) 给 Roomserver       │
│       → Gateway 再发送 JoinRoom(room_B) 给 Roomserver        │
│    5. 收到 ACK 后更新 localRooms，恢复一致                   │
└─────────────────────────────────────────────────────────────┘
```

### 7.4 Gateway 崩溃恢复

```
Gateway-1 崩溃
    ↓
Roomserver 感知 gRPC stream 断开（毫秒级）
    ↓
Roomserver 处理:
    1. 将 Gateway-1 上的所有用户标记为 disconnected
    2. 从 room.Gateways 中移除 Gateway-1
    3. 后续广播不再发给 Gateway-1
    ↓
客户端感知 WebSocket 断开，自动重连（可能连到 Gateway-2）
    ↓
客户端发送 Resume → 正常恢复
    ↓
超时未恢复的用户 → 自动从房间移除
```

### 7.5 Roomserver 崩溃恢复（RECOVERY MODE）

#### 恢复流程概览

```
┌──────────────────────────────────────────────────────────────────────────┐
│  1. Roomserver 启动                                                       │
│  2. epoch++（单调递增，标识本次启动）                                        │
│  3. enter RECOVERY MODE                                                   │
│  4. 向所有 Gateway 广播 epoch（轻量通知）                                    │
│  5. Gateway 检测 epoch 变化                                                │
│  6. Gateway 主动渐进式上报（BatchReSync + rate limit）                      │
│  7. Roomserver 合并状态                                                    │
│  8. 超时(T_recovery) OR 达到覆盖率 → 退出 RECOVERY MODE                     │
│  9. SERVING                                                               │
└──────────────────────────────────────────────────────────────────────────┘
```

#### 详细流程图

```
                    Roomserver 崩溃/重启
                          │
                          ▼
              ┌─────────────────────┐
              │  1. 新进程启动       │
              │  2. epoch++         │
              │     (如 epoch=5)    │
              │  3. RECOVERY MODE   │
              └──────────┬──────────┘
                         │
                         ▼
              ┌─────────────────────┐
              │  4. 广播 EpochNotify │
              │     {epoch: 5}      │
              └──────────┬──────────┘
                         │
        ┌────────────────┼────────────────┐
        ▼                ▼                ▼
   Gateway-1         Gateway-2         Gateway-3
   收到 epoch=5      收到 epoch=5      收到 epoch=5
   本地 epoch=4      本地 epoch=4      本地 epoch=4
   检测到变化！       检测到变化！       检测到变化！
        │                 │                 │
        ▼                 ▼                 ▼
   ┌─────────────────────────────────────────────┐
   │  6. 渐进式 BatchReSync（带 rate limit）       │
   │     - 按房间分批上报                          │
   │     - 每批 1000 用户                         │
   │     - 批次间隔 10ms                          │
   │     - 避免瞬间冲击 Roomserver                 │
   └─────────────────────────────────────────────┘
        │                 │                 │
        └────────────────┼────────────────┘
                         ▼
              ┌─────────────────────┐
              │  7. Roomserver 合并  │
              │     覆盖式重建状态    │
              │     统计恢复进度      │
              └──────────┬──────────┘
                         │
                         ▼
              ┌─────────────────────┐
              │  8. 退出条件判断     │
              │                     │
              │  条件 A: 超时        │
              │    T_recovery=30s   │
              │                     │
              │  条件 B: 覆盖率达标  │
              │    所有已连接        │
              │    Gateway 完成上报  │
              └──────────┬──────────┘
                         │
                         ▼
              ┌─────────────────────┐
              │  9. exit RECOVERY   │
              │  10. SERVING        │
              │      - 处理缓冲请求  │
              │      - 正常服务      │
              └─────────────────────┘
```

#### Epoch 机制

```go
type RoomServer struct {
    epoch     uint64        // 单调递增，每次启动 +1
    status    ServerStatus  // RECOVERING / SERVING
    // ...
}

// Epoch 通知消息（轻量）
type EpochNotify struct {
    Epoch     uint64 `msgpack:"epoch"`
    Timestamp int64  `msgpack:"ts"`
}

// Gateway 侧 epoch 检测
func (g *Gateway) onEpochNotify(notify EpochNotify) {
    if notify.Epoch > g.knownEpoch {
        g.knownEpoch = notify.Epoch
        // 触发渐进式 BatchReSync
        go g.startProgressiveReSync()
    }
}
```

#### 渐进式 BatchReSync

```go
const (
    ReSyncBatchSize     = 1000              // 每批用户数
    ReSyncBatchInterval = 10 * time.Millisecond  // 批次间隔
)

// Gateway 渐进式上报
func (g *Gateway) startProgressiveReSync() {
    // 按房间收集需要上报的用户
    roomUsers := g.collectConfirmedUsers()

    batch := make([]UserRoomPair, 0, ReSyncBatchSize)

    for roomID, users := range roomUsers {
        for _, userID := range users {
            batch = append(batch, UserRoomPair{
                UserID: userID,
                RoomID: roomID,
            })

            // 达到批次大小，发送并等待
            if len(batch) >= ReSyncBatchSize {
                g.sendBatchReSync(batch)
                batch = batch[:0]
                time.Sleep(ReSyncBatchInterval)  // rate limit
            }
        }
    }

    // 发送剩余数据
    if len(batch) > 0 {
        g.sendBatchReSync(batch)
    }

    // 发送完成标记
    g.sendReSyncComplete()
}
```

#### Roomserver 恢复状态管理

```go
type RecoveryState struct {
    StartTime       time.Time
    ExpectedGateways int                    // 预期 Gateway 数（从 Nacos 获取）
    CompletedGateways map[string]bool       // 已完成上报的 Gateway
    ReceivedUsers    int                    // 已接收用户数
}

func (r *RoomServer) onBatchReSync(gatewayID string, users []UserRoomPair) {
    // 合并状态
    for _, u := range users {
        r.mergeUserState(u.UserID, u.RoomID, gatewayID)
    }
    r.recovery.ReceivedUsers += len(users)
}

func (r *RoomServer) onReSyncComplete(gatewayID string) {
    r.recovery.CompletedGateways[gatewayID] = true
    r.checkRecoveryComplete()
}

func (r *RoomServer) checkRecoveryComplete() {
    // 条件 A: 超时
    if time.Since(r.recovery.StartTime) >= RecoveryTimeout {
        r.exitRecoveryMode("timeout")
        return
    }

    // 条件 B: 所有 Gateway 完成
    if len(r.recovery.CompletedGateways) >= r.recovery.ExpectedGateways {
        r.exitRecoveryMode("all_gateways_complete")
        return
    }
}

func (r *RoomServer) exitRecoveryMode(reason string) {
    log.Info("exit recovery mode",
        "reason", reason,
        "duration", time.Since(r.recovery.StartTime),
        "received_users", r.recovery.ReceivedUsers,
        "completed_gateways", len(r.recovery.CompletedGateways),
    )

    r.status = StatusServing
    r.processBufferedRequests()
}
```

#### RECOVERY MODE 行为

```
允许：
  - 接收 Gateway BatchReSync
  - 接收 JoinRoom/LeaveRoom（缓冲到队列）
  - 心跳、连接维持

限制：
  - 推送请求放入有界队列，满了返回 503
  - Python 后端收到 503 后指数退避重试
```

#### 配置参数

```go
const (
    RecoveryTimeout         = 30 * time.Second  // RECOVERY MODE 最长持续时间
    RecoveryPushQueueSize   = 10000             // 推送请求缓冲队列大小
    GatewayRequestQueueSize = 1000              // Gateway 请求缓冲队列大小
    ReSyncBatchSize         = 1000              // BatchReSync 每批用户数
    ReSyncBatchInterval     = 10 * time.Millisecond  // BatchReSync 批次间隔
)
```

#### 恢复期间的请求处理

```go
func (r *RoomServer) HandlePushRequest(req PushRequest) error {
    if r.status == StatusRecovering {
        if r.recoveryBuffer.TryPush(req) {
            return nil  // 缓冲成功
        }
        return ErrServiceRecovering  // 队列满，返回 503
    }
    return r.doPush(req)
}
```

---

## 八、服务发现与健康检测

### 8.1 双层机制

```
┌─────────────────────────────────────────────────────────────┐
│  实时故障检测（毫秒级）                                        │
│  Roomserver 与每个 Gateway 维持 gRPC stream 长连接              │
│  连接断开 = 立即感知（TCP FIN/RST → stream error）             │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│  服务发现（秒级，用于新节点加入/地址变更）                        │
│  Gateway/Roomserver 启动时注册到 Nacos                        │
│  订阅方收到 Nacos 推送后主动建立连接                             │
└─────────────────────────────────────────────────────────────┘
```

### 8.2 连接管理流程

```
Gateway 启动:
    1. 注册自身到 Nacos (imsys-gateway)
    2. 从 Nacos 订阅 imsys-roomserver
    3. 获取 Roomserver 实例列表，建立 gRPC stream 连接

Roomserver 启动:
    1. 注册自身到 Nacos (imsys-roomserver)
    2. 从 Nacos 订阅 imsys-gateway
    3. 等待 Gateway 主动连接

Gateway 优雅下线:
    1. 标记 DRAINING，不接新连接
    2. 通知客户端受控重连（分批，避免风暴）
    3. 从 Nacos 注销
    4. 关闭 gRPC stream
```

---

## 九、扩缩容

### 9.1 Dynamo 风格一致性哈希

```
hash(room_id) → 哈希环位置 → 顺时针第一个节点

扩容时只迁移 1/N 的数据，不是全部重新分配
```

### 9.2 扩容流程（简化版）

```
1. 新节点加入，更新一致性哈希配置（Nacos）
2. 计算需要迁移的房间列表
3. 逐个房间迁移：
   - 旧节点 → 新节点：发送房间数据
   - 旧节点：删除该房间
4. Gateway 路由：
   - 按本地缓存路由（可能是旧配置）
   - 旧节点没数据 → 返回 MOVED{new_node}
   - Gateway 更新缓存，重新请求新节点
5. 迁移完成后，Gateway 刷新路由配置
```

### 9.3 迁移期间写请求

```
房间正在迁移时：
  - 写请求返回 503
  - 单个房间迁移很快（毫秒级），影响极小
```

### 9.4 Gateway Draining（优雅下线）

```
1. 标记 DRAINING
2. 不接新连接
3. 通知客户端受控重连（分批，加 jitter）
4. 等待连接迁移完成
5. 关闭
```

---

## 十、性能优化

### 10.1 Gateway 广播扇出优化

```
优化层次：
  1. 消息压缩（减少带宽）
  2. Distributor 分片模式（隔离慢连接 + 并行写）
```

**Distributor 分片架构：**

```
                     payload (compressed)
                           ↓
                      Dispatcher
                           ↓
         ┌─────────────────┼─────────────────┐
         ↓                 ↓                 ↓
   Distributor-1     Distributor-2     Distributor-N
   (inputQueue)      (inputQueue)      (inputQueue)
         │                 │                 │
         ↓                 ↓                 ↓
   fanout goroutine (只分发，不做 IO)
         │
    ┌────┴────┬────┬────┐
    ↓         ↓    ↓    ↓
 Conn.sendCh → Conn.writeLoop (独立 goroutine，隔离慢连接)
```

**核心实现：**

```go
// Distributor：非阻塞分发
func (d *Distributor) run() {
    for payload := range d.inputQueue {
        for _, conn := range d.conns {
            select {
            case conn.sendCh <- payload:
                // 成功
            default:
                // 队列满，记录丢弃
                conn.dropCount++
                if conn.dropCount >= SlowConnThreshold {
                    conn.markSlowAndClose()
                }
            }
        }
    }
}

// Conn：独立写循环（带批量发送）
func (c *Conn) writeLoop() {
    ticker := time.NewTicker(BatchInterval)
    batch := make([][]byte, 0, BatchMaxSize)

    for {
        select {
        case msg, ok := <-c.sendCh:
            if !ok {
                c.flushBatch(batch)
                return
            }
            batch = append(batch, msg)
            if len(batch) >= BatchMaxSize {
                c.flushBatch(batch)
                batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                c.flushBatch(batch)
                batch = batch[:0]
            }
        }
    }
}
```

**设计要点：**

```
1. 分层职责
   - Distributor：CPU 密集（fanout），不阻塞
   - Conn.writeLoop：IO 密集，独立隔离

2. 非阻塞投递
   - select + default，队列满立即放弃
   - 保护快连接不受慢连接影响

3. 慢连接检测
   - 统计连续 dropCount
   - 超阈值主动关闭
```

---

## 十一、部署架构

### 11.1 实例规划（100K 在线）

| 组件 | 实例数 | 单实例负载 |
|------|--------|-----------|
| Gateway | 3-5 | 20K-35K 连接 |
| Roomserver | 2-3 | 通过一致性哈希分担房间 |
| Nacos | 3（集群） | 服务注册/配置 |
| Python 玩法后端 | 已有 | 不变 |

### 11.2 部署拓扑

```
                         ┌─── Gateway-1 ───┐
Client ── ELB (华为云) ──┼─── Gateway-2 ───┼──── Roomserver-1
                         └─── Gateway-3 ───┘     Roomserver-2
                               │                      ↑
                          GET /health            Python 玩法后端
                          (健康检查)
```

### 11.3 ELB 配置

```
监听器：
  - 前端协议：WebSocket (WSS)
  - 前端端口：443
  - 后端协议：WebSocket (WS)
  - 后端端口：8080

健康检查：
  - 协议：HTTP
  - 端口：8080
  - 路径：/health
  - 检查间隔：5s
  - 超时时间：3s
  - 健康阈值：2
  - 不健康阈值：3

会话保持：
  - 类型：源 IP
  - 保持时间：与 WebSocket 连接生命周期一致
```

---

## 十二、技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.21+ |
| 传输层 | gorilla/websocket（当前）、quic-go（未来） |
| 服务间通信 | gRPC（stream）+ msgpack（payload 编码） |
| 异步消息 | Kafka（Gateway → Python 业务请求转发） |
| 外部接口 | HTTP + JSON（供 Python 后端调用推送接口） |
| 序列化 | msgpack（内部）、JSON（外部） |
| 服务发现 | Nacos |
| 一致性哈希 | hashicorp/consistent 或自研 |
| 监控 | Prometheus + Grafana |
| 日志 | zap（结构化日志） |

---

## 十三、配置参数汇总

### 13.1 时间参数

| 参数 | 值 | 说明 |
|------|-----|------|
| RECOVERY MODE 超时 | 20s | Roomserver 恢复最长等待时间 |
| Disconnected 超时 | 60s | 用户断线后保留状态时间 |
| 心跳间隔 | 20s | 客户端心跳间隔（建议） |
| BatchReSync 批次间隔 | 10ms | 渐进式上报批次间隔（rate limit） |

### 13.2 队列/容量参数

| 参数 | 值 | 说明 |
|------|-----|------|
| Roomserver 推送队列 | 10000 | RECOVERING 期间推送缓冲 |
| Gateway 请求队列 | 1000 | RECOVERING 期间请求缓冲 |
| BatchReSync 批次大小 | 1000 | 渐进式上报每批用户数 |
| Distributor 分片数 | 10 | Gateway 连接分片数（可配置为 CPU 核数 × 2） |
| Conn SendCh 大小 | 1024 | 每连接下行消息队列 |
| 批量发送间隔 | 10ms | 消息聚合窗口 |
| 批量发送上限 | 10 | 单批最大消息数，达到立即发送 |

### 13.3 连接保护参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 队列预警阈值 | 50% | 开始丢弃 Low 优先级消息 |
| 队列严重阈值 | 80% | 开始丢弃 Normal 优先级消息 |
| 队列危险阈值 | 95% | 开始丢弃 High 优先级消息 |
| 队列满超时 | 30s | 队列持续满超过此时间断开连接 |
| 连续丢弃阈值 | 50 | 连续丢弃次数达到阈值断开连接 |
| 写超时阈值 | 500ms | 单次写操作超时判定 |
| 连续写超时阈值 | 10 | 连续写超时次数达到阈值断开连接 |

### 13.4 功能开关

| 功能 | 第一版 | 说明 |
|------|--------|------|
| 心跳弱自愈 | 开启 | 心跳带 room_id，检测不一致自动修复 |
| 消息分级 | 开启 | Critical/High/Normal/Low 四级 |
| 连接保护 | 开启 | 多维度慢连接检测与断开 |
| 消息压缩 | 开启 | 下行消息 zstd 压缩 |
| ACK 驱动更新 | 开启 | Gateway 状态由 ACK 驱动更新（无 Delta） |

---

## 十四、后续扩展预留

| 扩展点 | 当前方案 | 预留接口 |
|--------|---------|---------|
| QUIC 传输 | WebSocket | Transport 接口抽象 |
| 消息持久化 | 不持久化 | Message 结构预留持久化字段 |
| 多机房部署 | 单机房 | 一致性哈希可扩展为跨机房 |
| 消息加密 | TLS 传输加密 | Payload 层预留端到端加密字段 |
