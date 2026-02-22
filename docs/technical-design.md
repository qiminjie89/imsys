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
│  │ 连接管理  │ │ 协议编解码│ │ Kafka消费 │ │ 本地房间缓存(扇出) │  │
│  └──────────┘ └──────────┘ └──────────┘ └───────────────────┘  │
└──────────┬───────────────────────────────────┬─────────────────┘
           │ gRPC Stream                        │ Kafka
           │ (控制面：Join/Leave/Resume)         │ (数据面：消息推送)
           ▼                                    │
┌──────────────────────────────────┐            │
│   Roomserver (控制面，房间管理)    │            │
│  ┌───────────┐ ┌───────────┐    │            │
│  │RoomService│ │SessionSvc │    │            │
│  │(房间管理) │ │(会话管理)  │    │            │
│  └───────────┘ └───────────┘    │            │
└──────────────────────────────────┘            │
                                                │
                    ┌───────────────────────────┘
                    │ Kafka (im_msg_broadcast)
                    │ route_key = room_id / user_id
                    ▼
             ┌─────────────────────────────┐
             │   Python 玩法后端 (已有)      │
             │   (抽奖、礼物等玩法服务)       │
             │   - 消息推送直接写 Kafka      │
             │   - 维护消息 seq             │
             └─────────────────────────────┘

服务发现：Nacos（注册/发现，5s 刷新）
数据面与控制面分离：消息推送走 Kafka，房间操作走 Roomserver
```

### 3.1.1 数据面与控制面分离

```
┌─────────────────────────────────────────────────────────────────────┐
│  数据面（消息推送）- 热路径                                            │
├─────────────────────────────────────────────────────────────────────┤
│  Python 后端                                                         │
│      │                                                              │
│      │ 写入 Kafka (topic: im_msg_broadcast)                         │
│      │ - 房间广播：route_key = room_id                               │
│      │ - 单播：route_key = user_id                                   │
│      ▼                                                              │
│  Kafka                                                              │
│      │ 同一 room_id → 同一 partition → 保序                          │
│      ▼                                                              │
│  Gateway (每个实例订阅全部分区)                                        │
│      │ consumer_group = gateway_id                                  │
│      │ 检查 room_id/user_id 是否在本地                                │
│      │ - 在本地 → 推送给客户端                                        │
│      │ - 不在本地 → 丢弃                                             │
│      ▼                                                              │
│  Client                                                             │
│                                                                     │
│  优势：                                                              │
│    - Roomserver 短暂挂掉不影响消息推送                                │
│    - 减少一跳网络延迟                                                 │
│    - 热路径简化，性能更好                                             │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  控制面（房间操作）                                                   │
├─────────────────────────────────────────────────────────────────────┤
│  Client ↔ Gateway ↔ Roomserver                                      │
│                                                                     │
│  操作：JoinRoom / LeaveRoom / Resume                                │
│  特点：相对低频，允许短暂中断                                         │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 请求流向

| 场景 | 链路 | 说明 |
|------|------|------|
| 客户端业务请求 | Client → Gateway → Kafka → Python | 抽奖、送礼等业务请求 |
| 房间操作 | Client → Gateway → Roomserver | JoinRoom / LeaveRoom（控制面）|
| 恢复连接 | Client → Gateway → Roomserver | Resume（控制面）|
| 房间广播 | Python → Kafka → Gateway → Client | route_key=room_id（数据面）|
| 单播消息 | Python → Kafka → Gateway → Client | route_key=user_id（数据面）|
| 全局广播 | Python → Kafka → Gateway → Client | 所有 Gateway 消费并推送（数据面）|

---

## 四、组件详细设计

### 4.1 Gateway（接入层）

#### 核心职责

- 客户端 WebSocket 连接管理（建连、心跳、断线检测）
- 轻量鉴权（JWT 本地验证，RS256 签名，仅首次连接校验）
- 协议编解码（消息封包/拆包）
- **数据面**：
  - 消费 Kafka `im_msg_broadcast` topic（独立 consumer group = gateway_id）
  - 根据 room_id/user_id 判断是否在本地，过滤后推送给客户端
  - 客户端异步请求 → Kafka → Python 后端
- **控制面**：
  - 房间操作（JoinRoom/LeaveRoom/Resume）→ Roomserver
  - 用户断连 → 同步通知 Roomserver
- 本地房间成员缓存（用于消息过滤和广播扇出）
- sendQueue + 背压控制
- 向 Nacos 注册自身服务信息（服务名：gateway）
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

三种状态：
  - healthy: 完全正常，可接新连接
  - degraded: 控制面不可用，数据面正常，不接新连接
  - unhealthy: 数据面不可用，不接新连接

响应示例（健康）：
HTTP 200 OK
{
    "status": "healthy",
    "connections": 12500,
    "roomserver_connected": true,
    "kafka_connected": true,
    "uptime_seconds": 3600
}

响应示例（降级 - Roomserver 断连）：
HTTP 200 OK
{
    "status": "degraded",
    "reason": "roomserver_disconnected",
    "degraded_since": "2024-01-15T10:30:00Z",
    "connections": 12500,
    "roomserver_connected": false,
    "kafka_connected": true
}

响应示例（不健康 - Kafka 断连）：
HTTP 503 Service Unavailable
{
    "status": "unhealthy",
    "reason": "kafka_disconnected",
    "connections": 12500,
    "roomserver_connected": true,
    "kafka_connected": false
}

ELB 配置：
  - healthy (200 + status=healthy) → 分配新连接
  - degraded (200 + status=degraded) → 不分配新连接，保持现有连接
  - unhealthy (503) → 不分配新连接，考虑踢连接
```

```go
// 健康检查实现
func (g *Gateway) HealthHandler(w http.ResponseWriter, r *http.Request) {
    health := &HealthStatus{
        Connections:         len(g.connections),
        RoomserverConnected: g.isRoomserverConnected(),
        KafkaConnected:      g.kafkaConsumer.IsConnected(),
        UptimeSeconds:       time.Since(g.startTime).Seconds(),
    }

    if !health.KafkaConnected {
        // Kafka 断连 = 数据面不可用 = unhealthy
        health.Status = "unhealthy"
        health.Reason = "kafka_disconnected"
        w.WriteHeader(http.StatusServiceUnavailable)
    } else if !health.RoomserverConnected {
        // Roomserver 断连 = 控制面不可用 = degraded
        health.Status = "degraded"
        health.Reason = "roomserver_disconnected"
        health.DegradedSince = g.degradedSince
        w.WriteHeader(http.StatusOK)  // 200，但 ELB 根据 status 判断
    } else {
        health.Status = "healthy"
        w.WriteHeader(http.StatusOK)
    }

    json.NewEncoder(w).Encode(health)
}
```

#### 数据结构

```go
type Gateway struct {
    // Gateway 配置（启动时从配置文件读取）
    gatewayID string  // 配置项：gateway.id
    ip        string  // 配置项：gateway.ip
    port      int     // 配置项：gateway.port

    // 连接管理（不支持多端登录，user_id 唯一）
    connections map[string]*Connection  // user_id → connection

    // Distributor 分片（广播扇出）
    distributors []*Distributor  // 按 hash(user_id) 分配连接

    // 本地房间成员缓存（用于消息过滤和广播扇出，ACK 驱动更新）
    localRooms map[string]map[string]bool  // room_id → Set<user_id>

    // 本地用户索引（用于单播消息快速判断）
    localUsers map[string]bool  // user_id → true

    // 与 Roomserver 的 gRPC stream（启动时连接所有 Roomserver）
    roomserverStreams map[string]*grpc.ClientStream  // roomserver_id → stream

    // 各 Roomserver 实例的 epoch（用于检测重启）
    knownEpochs map[string]uint64  // roomserver_id → epoch

    // 一致性哈希环（用于预测路由到 Roomserver）
    hashRing *ConsistentHash

    // Kafka 生产者（业务请求转发给 Python）
    kafkaProducer *kafka.Producer

    // Kafka 消费者（消费推送消息）
    // consumer_group = gateway_id（每个 Gateway 独立消费全部消息）
    kafkaConsumer *kafka.Consumer

    // 用户请求串行化锁（保证同一用户的 Join/Leave 顺序）
    userLocks sync.Map  // user_id → *sync.Mutex
}

// Gateway 配置示例（gateway.yaml）：
// gateway:
//   id: "gw-1"                    # Gateway 唯一标识
//   ip: "192.168.1.10"            # 监听 IP
//   port: 8080                    # 监听端口
//   kafka:
//     consumer_group: "gw-1"      # 通常与 gateway.id 相同
//     topic: "im_msg_broadcast"   # 订阅的推送 topic

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
    sendCh      chan []byte       // 下行消息队列
    dropCount   int               // 连续丢弃次数（慢连接检测）
    JoinState   JoinState         // JoinRoom 状态机
    RoomID      string            // 当前所在房间
    AuthTime    time.Time         // 认证时间
    TokenExpiry time.Time         // Token 过期时间（用于定时检查）
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

#### 用户请求串行化

```
问题：用户 JoinRoom(B) 时，可能还在 Room(A)
解决：Gateway 层对同一用户的请求加锁串行化

处理流程：
  1. Gateway 收到 JoinRoom(B) 请求
  2. 获取 userLocks[user_id] 锁
  3. 检查用户当前状态：
     - 如果用户在 Room(A)（CONFIRMED）：
       a. 先发送 LeaveRoom(A) 给 Roomserver
       b. 等待 LeaveRoom ACK
       c. 更新本地状态
     - 如果用户在 PENDING 状态：
       a. 等待 PENDING 完成或超时
  4. 发送 JoinRoom(B) 给 Roomserver
  5. 等待 JoinRoom ACK
  6. 释放锁

代码示例：
  func (g *Gateway) handleJoinRoom(conn *Connection, roomID string) {
      lock := g.getUserLock(conn.UserID)
      lock.Lock()
      defer lock.Unlock()

      // 如果已在其他房间，先离开
      if conn.RoomID != "" && conn.JoinState == CONFIRMED {
          g.doLeaveRoom(conn, conn.RoomID)
      }

      g.doJoinRoom(conn, roomID)
  }
```

#### 路由预测（Gateway → Roomserver）

```
┌─────────────────────────────────────────────────────────────────────┐
│  Gateway 启动时                                                      │
├─────────────────────────────────────────────────────────────────────┤
│  1. 从 Nacos 订阅 Roomserver 服务列表                                │
│  2. 与所有 Roomserver 建立 gRPC stream 连接                          │
│  3. 本地构建一致性哈希环（用于路由预测）                               │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  路由策略：预测 + 转发                                                │
├─────────────────────────────────────────────────────────────────────┤
│  Gateway 发送 JoinRoom/LeaveRoom 请求时：                            │
│    1. 计算 hash(room_id) → 预测目标 Roomserver                       │
│    2. 发送请求到预测的 Roomserver                                    │
│                                                                     │
│  Roomserver 收到请求后：                                             │
│    - 如果 room_id 归属本实例 → 直接处理                               │
│    - 如果不归属本实例 → 内部转发到正确节点                             │
│                                                                     │
│  设计要点：                                                          │
│    - Gateway 不做强一致路由，只做【预测】                             │
│    - Roomserver 保证最终正确性                                       │
│    - 扩缩容时 Gateway 哈希环可能短暂过期，由 Roomserver 兜底          │
└─────────────────────────────────────────────────────────────────────┘
```

#### 鉴权流程（AUTH）

```
┌─────────────────────────────────────────────────────────────────────┐
│  认证流程                                                            │
├─────────────────────────────────────────────────────────────────────┤
│  1. 客户端从 Python 后端获取 token（登录流程）                         │
│  2. 客户端建立 WebSocket 连接                                        │
│  3. 发送 AUTH 消息携带 token（JWT）                                  │
│  4. Gateway 本地验证 JWT：                                           │
│     - 签名算法：RS256                                                │
│     - 公钥来源：Nacos 配置中心                                        │
│     - 签名校验                                                       │
│     - 过期时间检查                                                   │
│     - 提取 user_id (sub) 和 expiry (exp)                            │
│  5. 验证通过：                                                       │
│     - 绑定 user_id                                                  │
│     - 记录 TokenExpiry                                              │
│     - 启动该连接的过期检查定时器                                      │
│     - 返回 AUTH_SUCCESS                                             │
│  6. 验证失败：                                                       │
│     - 返回 AUTH_FAILED                                              │
│     - 断开连接                                                       │
│  7. 后续请求不再校验 token                                           │
└─────────────────────────────────────────────────────────────────────┘

JWT Claims 规范：
{
    "sub": "user_id",        // 用户 ID（MongoDB _id）
    "exp": 1707123456,       // 过期时间（Unix 时间戳）
    "iat": 1707120000,       // 签发时间
    "iss": "auth-service"    // 签发者
}

签名配置：
  - 算法：RS256
  - 公钥存储：Nacos 配置中心
  - 配置 Key：jwt.public_key
```

┌─────────────────────────────────────────────────────────────────────┐
│  Token 过期处理                                                      │
├─────────────────────────────────────────────────────────────────────┤
│  Gateway 为每个连接维护轻量定时器：                                    │
│                                                                     │
│  定时检查（每分钟）：                                                 │
│    if now > conn.TokenExpiry:                                       │
│        conn.Close("token_expired")                                  │
│        // 注意：不触发 LeaveRoom！                                   │
│        // 用户可能正在刷新 token 并重连                               │
│                                                                     │
│  客户端收到断连后：                                                   │
│    1. 从 Python 后端 refresh token                                  │
│    2. 重新建连 + AUTH                                               │
│    3. 发送 Resume 恢复房间状态                                       │
│                                                                     │
│  重连超时（60s）后才触发 LeaveRoom                                   │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  客户端退出 App                                                      │
├─────────────────────────────────────────────────────────────────────┤
│  客户端退出 App → WebSocket 断开 → Gateway 感知 → 触发 LeaveRoom     │
└─────────────────────────────────────────────────────────────────────┘
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

### 4.2 Roomserver（控制面服务）

#### 核心职责

- **仅负责控制面**（消息推送已改为 Kafka 直通）
- 房间管理（用户进出房间、房间成员维护）
- 会话管理（用户与 Gateway 的映射关系）
- 用户断连状态管理（disconnected 超时处理）
- 内部转发（路由过时时，转发请求到正确节点）
- 控制面裁决权威
- 向 Nacos 注册自身服务信息（服务名：roomserver）

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
    RoomID   string  // MongoDB _id，全局唯一

    // 房间成员
    Members  map[string]*RoomMember  // user_id → member
    mu       sync.RWMutex            // 保护 Members

    // 房间内 Gateway 列表（用于 RoomClose 通知）
    Gateways map[string]*GatewayRoomView  // gateway_id → 该 Gateway 上的用户列表
}

// 房间生命周期：
//   - 创建：主播开播时，Python 业务进程创建房间
//   - 销毁：主播关播时，Python 调用 RoomClose 接口
//   - room_id：使用 MongoDB _id，每个房间唯一
//   - Roomserver 的 Room 对象在首个用户 JoinRoom 时懒创建，RoomClose 时销毁
//
// 消息 seq：
//   - 由 Python 业务进程管理（不在 Roomserver）
//   - 消息直接通过 Kafka 推送，不经过 Roomserver

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
+----------+----------+----------+----------+------------------+
|  Magic   | Version  |  MsgType | BodyLen  |     Payload      |
|  2 bytes |  1 byte  |  4 bytes | 4 bytes  |   变长 (msgpack)  |
+----------+----------+----------+----------+------------------+

字段说明：
  - Magic:   固定值 0x494D (ASCII "IM")，用于帧边界检测
  - Version: 协议版本，当前为 0x01
  - MsgType: 消息类型编号
  - BodyLen: Payload 长度
  - Payload: msgpack 序列化的消息体

字节序：大端（Big-Endian）
```

#### 推送消息体结构

```go
// 推送消息的 Payload 结构
type PushPayload struct {
    Scope   string `msgpack:"scope"`    // "room" | "user" | "platform"
    RoomID  string `msgpack:"room_id"`  // scope=room 时必填
    UserID  string `msgpack:"user_id"`  // scope=user 时必填
    Seq     uint64 `msgpack:"seq"`      // 由 Python 业务进程生成
    Message []byte `msgpack:"message"`  // 业务消息内容
}

// Seq 作用域：
//   - scope=room: 房间级 seq，用于房间内消息排序
//   - scope=user: 用户级 seq，用于单播消息排序
//   - scope=platform: 平台级 seq，用于全局广播排序
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
    MsgTypeHeartbeat  = 0x00FF  // 心跳（携带 room_id 用于弱自愈）

    // Gateway → 客户端
    MsgTypeAuthResp      = 0x1001  // 认证响应
    MsgTypeJoinRoomResp  = 0x1002  // 加入房间响应
    MsgTypeLeaveRoomResp = 0x1003  // 离开房间响应
    MsgTypeResumeResp    = 0x1004  // Resume 响应
    MsgTypePushMessage   = 0x1010  // 推送消息（包含 scope/seq）
    MsgTypeBizResponse   = 0x1011  // 业务响应
    MsgTypeHeartbeatResp = 0x10FF  // 心跳响应（包含状态检查结果）
)

// 心跳请求
type HeartbeatRequest struct {
    RoomID string `msgpack:"room_id"`  // 客户端当前认为所在的房间
}

// 心跳响应
type HeartbeatResponse struct {
    Status        string `msgpack:"status"`          // "ok" | "mismatch" | "not_in_room"
    GatewayRoomID string `msgpack:"gateway_room_id"` // Gateway 记录的 room_id
    ClientRoomID  string `msgpack:"client_room_id"`  // 客户端声称的 room_id
}
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

#### 错误码定义

```go
const (
    // 通用错误
    ErrCodeSuccess           = 0      // 成功
    ErrCodeInternalError     = 1      // 内部错误
    ErrCodeInvalidRequest    = 2      // 无效请求

    // 认证相关
    ErrCodeAuthFailed        = 1001   // 认证失败
    ErrCodeTokenExpired      = 1002   // Token 过期

    // 房间相关
    ErrCodeRoomNotFound      = 2001   // 房间不存在
    ErrCodeUserNotInRoom     = 2002   // 用户不在房间
    ErrCodeRoomClosed        = 2003   // 房间已关闭
    ErrCodeRoomMismatch      = 2004   // Resume 时 room_id 不匹配

    // 路由相关
    ErrCodeOwnerUnavailable  = 3001   // 目标 Roomserver 不可用（内部转发失败）

    // 心跳相关
    ErrCodeHeartbeatMismatch = 4001   // 心跳检测到 room_id 不一致
    ErrCodeHeartbeatNotInRoom = 4002  // 心跳检测到用户不在房间

    // 服务状态相关
    ErrCodeServiceRecovering = 5001   // 服务恢复中
    ErrCodeServiceOverloaded = 5002   // 服务过载
)
```

**OWNER_UNAVAILABLE 处理：**

```
场景：Roomserver 内部转发失败

当 Roomserver-1 收到请求，发现 room_id 不归属自己：
  1. 计算 hash(room_id) → 应路由到 Roomserver-2
  2. 尝试内部转发给 Roomserver-2
  3. 如果转发失败（Roomserver-2 不可用）：
     → 立即返回 OWNER_UNAVAILABLE
     → 不做无效重试

客户端/Gateway 收到 OWNER_UNAVAILABLE：
  - 表示目标 Roomserver 暂时不可用
  - 可稍后重试（由客户端/业务方决定）
  - 设计原则：快速失败，不阻塞
```

### 5.3 Gateway / Python 玩法后端 通信

#### 5.3.1 业务请求（Gateway → Kafka → Python）

```
客户端发送业务请求（抽奖、送礼等）：
  Client → Gateway → Kafka → Python 后端

Kafka Topic: im_biz_request（业务请求）

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
  - Python 后端按需处理，结果通过推送 Kafka 返回
  - 解耦 Go IM 系统与 Python 业务系统
```

#### 5.3.2 消息推送（Python → Kafka → Gateway）

```
消息推送路径（数据面，不经过 Roomserver）：
  Python 后端 → Kafka → Gateway → Client

Kafka Topic: im_msg_broadcast（消息广播）

Kafka 配置：
  - Topic: im_msg_broadcast
  - Partition Key:
    - 房间广播/多播: room_id（保证同房间消息顺序）
    - 单播: user_id
  - 每个 Gateway 用自己的 gateway_id 作为 consumer_group
  - 每个 Gateway 订阅全部 partition

Kafka 消息格式：
{
    "scope": "room",           // "room" | "user" | "platform"
    "room_id": "xxx",          // scope=room/multicast 时必填
    "user_id": "yyy",          // scope=user 时必填
    "user_ids": ["a", "b"],    // scope=multicast 时必填
    "seq": 12345,              // Python 业务进程生成，用于保序
    "priority": "normal",      // critical/high/normal/low
    "message": {
        "type": "gift_animation",
        "data": { ... }
    }
}

Gateway 消费逻辑：
  1. 从 Kafka 消费消息
  2. 根据 scope 判断处理方式：
     - scope=room: 检查 localRooms[room_id] 是否存在
     - scope=user: 检查 localUsers[user_id] 是否存在
     - scope=platform: 推送给所有在房间内的用户
  3. 匹配则推送，不匹配则丢弃

保序机制：
  - 同一 room_id 作为 partition key → 同一 partition
  - 同一 partition 由同一 consumer 消费 → 有序
  - Python 维护 seq，客户端可用于排序/去重
```

#### 5.3.3 房间关闭接口（Python → Roomserver，HTTP + JSON）

```
房间关闭仍走 Roomserver（控制面操作）：

POST /api/v1/room/close
{
    "room_id": "xxx",
    "reason": "ended"  // ended, banned, etc.
}

响应：
{
    "success": true,
    "room_id": "xxx",
    "affected_users": 5000,
    "failed_gateways": []  // 通知失败的 Gateway 列表
}

执行参数：
  - 在独立 goroutine 中执行，不阻塞其他 Roomserver 操作
  - 单 Gateway 通知超时：3s
  - 重试次数：2 次（共 3 次尝试）
  - 重试间隔：500ms
  - 总超时上限：10s

路由策略：
  Python → hash(room_id) → Roomserver
  如果路由过时，Roomserver 内部转发到正确节点
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

**断连（同步通知 Roomserver）：**

```
WebSocket 断开（单个用户，Gateway 仍在运行）
      │
      ▼
Gateway-1:
    1. 同步发送 Disconnect{user_id, room_id} 给 Roomserver
    2. 等待 Roomserver 确认
      │
      ├── 成功 ──────────────────────────────────────────┐
      │                                                  │
      ├── 失败 → 重试（最多 3 次，间隔 500ms）             │
      │            │                                     │
      │            └── 重试失败 → 记录日志，继续执行       │
      │                                                  │
      ▼                                                  │
Gateway-1 更新本地状态：                                  │
    1. localRooms[room].remove(user)  ←──────────────────┘
    2. localUsers.remove(user)
      │
      ▼
Roomserver 处理 Disconnect:
    1. 标记 user.Status = Disconnected
    2. 启动 60s 超时计时器
    3. 超时后自动 LeaveRoom

设计要点：
  - 同步通知确保 Roomserver 先感知断连
  - Gateway 等待确认后才更新本地状态
  - 失败重试保证可靠性
  - 重试仍失败时，Gateway 本地状态仍需清理（避免内存泄漏）
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

### 6.2 广播消息流程（Kafka 直通）

#### 房间广播

```
Python 后端:
    1. 生成 seq（房间级单调递增）
    2. 写入 Kafka:
       Topic: im_msg_broadcast
       Key: room_id (保证同房间消息有序)
       Value: {scope:"room", room_id:X, seq:N, message:M}
    ↓
Kafka:
    同一 room_id → 同一 partition → 有序投递
    ↓
Gateway (所有实例都消费):
    1. 检查 localRooms[X] 是否存在
    2. 存在 → 查 localRooms[X] → [user_1, user_2, ...]
    3. Distributor 本地扇出：遍历用户推送消息
    4. 不存在 → 丢弃（该房间不在本 Gateway）
```

#### 单播

```
Python 后端:
    1. 生成 seq（用户级）
    2. 写入 Kafka:
       Topic: im_msg_broadcast
       Key: user_id
       Value: {scope:"user", user_id:A, seq:N, message:M}
    ↓
Gateway (所有实例都消费):
    1. 检查 localUsers[A] 是否存在
    2. 存在 → 查 connections[A] → conn.SendQueue <- M
    3. 不存在 → 丢弃（该用户不在本 Gateway）

注意：单播时所有 Gateway 都会消费消息，只有目标 Gateway 处理，其他丢弃
      这是简化架构的权衡（避免维护 user→gateway 的全局映射）
```

#### 多播（Multicast）

```
场景：发送消息给房间内指定的一组用户
  - 中奖通知（只发给中奖者）
  - 特定用户组的私密通知

Python 后端:
    1. 写入 Kafka:
       Topic: im_msg_broadcast
       Key: room_id (保证同房间消息有序)
       Value: {scope:"multicast", room_id:X, user_ids:[A,B,C], seq:N, message:M}
    ↓
Gateway (所有实例都消费):
    1. 检查 localRooms[X] 是否存在
    2. 存在 → 遍历 user_ids，检查是否在 localUsers
    3. 在本地的用户 → 推送
    4. 不在本地的用户 → 跳过
```

#### 全局广播

```
Python 后端:
    1. 写入 Kafka:
       Topic: im_msg_broadcast
       Key: "platform" (或空)
       Value: {scope:"platform", seq:N, message:M}
    ↓
Gateway (所有实例都消费):
    1. scope=platform → 遍历 localRooms 所有房间
    2. 推送给所有在房间内的用户
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

#### 整体保序链路（Kafka 直通）

```
消息源（Python后端）
    ↓ Python 进程维护 seq，写入消息顺序 = 最终顺序
Kafka（同一 room_id → 同一 partition）  ← 唯一串行化点
    ↓ Partition 内有序消费
Gateway（Distributor 扇出，不负责保序）
    ↓ WebSocket（TCP，有序）
客户端（按 room_id + seq 排序/去重）
```

#### 保序机制

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Python 业务进程                                                         │
│                                                                         │
│  维护每个房间的 seq：                                                    │
│    room_seqs = {"room_123": 100, "room_456": 50, ...}                  │
│                                                                         │
│  发送广播时：                                                            │
│    1. seq = room_seqs[room_id]++                                       │
│    2. 写入 Kafka:                                                       │
│       Topic: im_msg_broadcast                                          │
│       Key: room_id  ← 保证同房间消息进入同一 partition                   │
│       Value: {scope:"room", room_id, seq, message}                     │
└─────────────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Kafka                                                                  │
│                                                                         │
│  Partition 0: room_123, room_789, ...                                  │
│  Partition 1: room_456, room_321, ...                                  │
│  Partition N: ...                                                       │
│                                                                         │
│  同一 room_id 的消息 → 同一 partition → 有序                             │
└─────────────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Gateway (每个实例独立消费全部 partition)                                 │
│                                                                         │
│  consumer_group = gateway_id (独立消费)                                  │
│                                                                         │
│  消费逻辑：                                                              │
│    1. 检查 room_id 是否在 localRooms                                    │
│    2. 在 → Distributor 扇出 → 客户端                                    │
│    3. 不在 → 丢弃                                                       │
└─────────────────────────────────────────────────────────────────────────┘

消息格式：
  {
      "scope": "room",
      "room_id": "room_123",
      "seq": 101,
      "message": { ... }
  }

客户端处理：
  - 按 room_id 分组
  - 同一房间按 seq 排序/去重
```

**优势：**

```
1. 数据面与控制面解耦
   - Roomserver 短暂挂掉不影响消息推送
   - 消息推送热路径更短（少一跳）

2. 水平扩展简单
   - Kafka partition 数量可动态调整
   - Gateway 无状态，可随时扩缩容

3. 保序机制清晰
   - Python 维护 seq，责任单一
   - Kafka partition 保证顺序
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

**消息重放策略（第一版）：**

```
第一版设计：Resume 不做消息重放

理由：
  - 直播场景消息时效性强，过期消息价值低
  - 断连期间消息量可能很大（高频弹幕）
  - 实现消息缓存增加系统复杂度
  - 客户端可通过业务接口主动拉取关键状态

Resume 响应：
  {
      "success": true,
      "room_id": "xxx",
      "missed_messages": 0,  // 第一版固定为 0，预留字段
      // "replay_token": ""  // 预留：未来支持消息重放时使用
  }

后续扩展预留：
  - missed_messages: 断连期间错过的消息数
  - replay_token: 用于拉取历史消息的凭证
  - 可通过独立接口 GET /api/v1/messages/replay?token=xxx 实现
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

409 Conflict (Room Mismatch)
  - 客户端 Resume 携带的 room_id 与 Roomserver 记录不一致
  - 响应中包含 Roomserver 认为的 current_room_id
  - 客户端：先 LeaveRoom(current_room_id)，再 JoinRoom(目标房间)

410 Room Closed
  - 用户断连期间房间已关闭（RoomClose）
  - 客户端：提示用户房间已结束

Resume 响应结构：
{
    "success": false,
    "error_code": 409,
    "current_room_id": "xxx",  // 仅 409 时返回
    "message": "room_mismatch"
}
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
│  → 正常响应     → 返回错误码，告知客户端              → 返回错误│
│                   自行执行 Leave/Join                  码     │
│                                                             │
│  设计要点（不阻塞心跳处理）：                                 │
│    1. Gateway 不代替客户端执行 Leave/Join                    │
│    2. 仅返回错误码 + 当前 Gateway 记录的 room_id              │
│    3. 客户端收到错误码后自行决定是否执行 Leave/Join           │
│                                                             │
│  心跳响应结构：                                              │
│    {                                                        │
│        "status": "ok" | "mismatch" | "not_in_room",         │
│        "gateway_room_id": "xxx",  // Gateway 记录的 room_id  │
│        "client_room_id": "yyy"    // 客户端声称的 room_id    │
│    }                                                        │
│                                                             │
│  客户端处理：                                                │
│    - mismatch: Leave(gateway_room_id) → Join(client_room_id)│
│    - not_in_room: Join(client_room_id)                      │
│    - Join 失败：提示用户进房失败                              │
└─────────────────────────────────────────────────────────────┘
```

### 7.4 Gateway 与 Roomserver 网络分区

#### 问题场景

```
Gateway 连不上 Roomserver，可能原因：
  1. 网络分区（Gateway ↔ Roomserver 网络断开）
  2. Roomserver 真的挂了
  3. Roomserver 过载无法响应

关键点：Gateway 无法区分这三种情况
```

#### 处理策略：不踢用户 + 分级降级

```
核心原则：
  - 踢用户没有意义（连不上 Roomserver，重连也无法 JoinRoom）
  - 数据面（Kafka）独立，消息推送不受影响
  - 保证数据面可用，控制面优雅降级，等待恢复而不是雪崩

┌─────────────────────────────────────────────────────────────────────┐
│  阶段 1：短期断连（< 30s）                                            │
├─────────────────────────────────────────────────────────────────────┤
│  行为：                                                              │
│    - Gateway 状态：NORMAL（尝试重连中）                               │
│    - 控制面请求返回 503，提示客户端稍后重试                            │
│    - 数据面正常（Kafka 消息推送）                                     │
│    - Gateway 持续尝试重连 Roomserver                                 │
│                                                                     │
│  客户端处理：                                                        │
│    - JoinRoom 失败 → 退避重试（1s, 2s, 4s...）                       │
│    - 已在房间用户 → 继续收消息，无感知                                │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  阶段 2：长期断连（>= 30s）                                           │
├─────────────────────────────────────────────────────────────────────┤
│  行为：                                                              │
│    - Gateway 状态：DEGRADED                                          │
│    - /health 返回 degraded（ELB 不分配新连接）                        │
│    - 数据面继续正常                                                  │
│    - 控制面降级处理                                                  │
│    - 不踢用户                                                        │
│    - 触发告警，运维介入                                               │
│                                                                     │
│  控制面降级策略：                                                    │
│    - JoinRoom：返回 503，无法处理                                    │
│    - LeaveRoom：本地执行，记录 pendingLeaves 待补偿                   │
│    - Resume：检查 localRooms，有则成功，无则返回 503                  │
│    - 用户断连：本地清理，记录 pendingDisconnects 待补偿               │
└─────────────────────────────────────────────────────────────────────┘
```

#### DEGRADED 模式实现

```go
type GatewayStatus int
const (
    StatusNormal GatewayStatus = iota
    StatusDegraded
)

type Gateway struct {
    status           GatewayStatus
    degradedSince    time.Time
    pendingLeaves    []PendingRequest  // 待补偿的 LeaveRoom
    pendingDisconnects []PendingRequest  // 待补偿的用户断连
}

// 控制面请求处理
func (g *Gateway) handleControlPlane(req Request) Response {
    if g.status == StatusDegraded {
        switch req.Type {
        case JoinRoom:
            // 无法处理，返回错误
            return Response{Code: 503, Msg: "service_degraded"}

        case LeaveRoom:
            // 本地执行，记录待补偿
            g.localLeave(req.UserID, req.RoomID)
            g.pendingLeaves = append(g.pendingLeaves, req)
            return Response{Code: 200, Msg: "accepted_degraded"}

        case Resume:
            // 本地判断
            if g.localRooms[req.RoomID][req.UserID] {
                return Response{Code: 200}
            }
            return Response{Code: 503, Msg: "service_degraded"}
        }
    }
    return g.forwardToRoomserver(req)
}
```

#### 恢复流程

```
Gateway 重连 Roomserver 成功：
    │
    ├── 1. 发送 BatchReSync（同步本地状态到 Roomserver）
    │
    ├── 2. 处理 pendingLeaves（补偿降级期间的 LeaveRoom）
    │
    ├── 3. 处理 pendingDisconnects（补偿降级期间的用户断连）
    │
    ├── 4. 清空待补偿队列
    │
    ├── 5. 标记 status = NORMAL
    │
    └── 6. /health 返回 healthy（ELB 恢复分配新连接）
```

#### 健康检查

```go
func (g *Gateway) HealthHandler() HealthStatus {
    status := HealthStatus{
        RoomserverConnected: g.isRoomserverConnected(),
        KafkaConnected:      g.kafkaConsumer.IsConnected(),
    }

    if !status.KafkaConnected {
        // Kafka 断开 = 数据面不可用 = unhealthy
        status.Status = "unhealthy"
        return status
    }

    if !status.RoomserverConnected {
        // Roomserver 断开 = 控制面不可用 = degraded
        status.Status = "degraded"
        status.DegradedSince = &g.degradedSince
        status.DegradedReason = "roomserver_disconnected"
        return status
    }

    status.Status = "healthy"
    return status
}

// ELB 健康检查配置：
//   - healthy → 正常分配新连接
//   - degraded → 不分配新连接，但不踢现有连接
//   - unhealthy → 不分配，考虑踢连接
```

### 7.5 Gateway 崩溃恢复

```
Gateway-1 崩溃
    ↓
Roomserver 感知 gRPC stream 断开（毫秒级）
    ↓
Roomserver 处理:
    1. 将 Gateway-1 上的所有用户标记为 disconnected
    2. 从 room.Gateways 中移除 Gateway-1
    ↓
客户端感知 WebSocket 断开，自动重连（可能连到 Gateway-2）
    ↓
客户端发送 Resume → 正常恢复
    ↓
超时未恢复的用户 → 自动从房间移除
```

### 7.6 Roomserver 崩溃恢复（RECOVERY MODE）

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

```
Epoch 定义：
  - Epoch = Roomserver 实例启动时的毫秒时间戳
  - 每个 Roomserver 实例独立维护自己的 epoch
  - 用于 Gateway 检测 Roomserver 是否发生重启

Gateway 维护方式：
  - knownEpochs: map[roomserver_id]uint64
  - 每个 Roomserver 实例对应一个 epoch 记录
  - 例如：{"rs-1": 1707123456789, "rs-2": 1707123400000}
```

```go
type RoomServer struct {
    serverID  string        // 实例 ID
    epoch     uint64        // 启动时间戳（毫秒）
    status    ServerStatus  // RECOVERING / SERVING
    // ...
}

// 启动时设置 epoch
func NewRoomServer() *RoomServer {
    return &RoomServer{
        epoch: uint64(time.Now().UnixMilli()),
        // ...
    }
}

// Epoch 通知消息（轻量）
type EpochNotify struct {
    ServerID  string `msgpack:"server_id"`
    Epoch     uint64 `msgpack:"epoch"`
    Timestamp int64  `msgpack:"ts"`
}

// Gateway 侧 epoch 检测（按 server_id 区分）
func (g *Gateway) onEpochNotify(notify EpochNotify) {
    oldEpoch := g.knownEpochs[notify.ServerID]
    if notify.Epoch != oldEpoch {
        g.knownEpochs[notify.ServerID] = notify.Epoch
        // 触发对该 Roomserver 的渐进式 BatchReSync
        go g.startProgressiveReSync(notify.ServerID)
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
    // 合并状态（以最新的 Gateway 为准）
    for _, u := range users {
        r.mergeUserState(u.UserID, u.RoomID, gatewayID)
    }
    r.recovery.ReceivedUsers += len(users)
}

// 冲突处理策略：
//   1. 同一用户被多个 Gateway 上报：以最新（最后收到）的 Gateway 为准
//   2. BatchReSync 与正常 JoinRoom 请求交叉：以最新的请求为准
//   3. 合并逻辑：直接覆盖，不做版本比较（简化实现）
func (r *RoomServer) mergeUserState(userID, roomID, gatewayID string) {
    // 直接覆盖：最新的请求总是生效
    r.users[userID] = &UserState{
        UserID:    userID,
        RoomID:    roomID,
        GatewayID: gatewayID,
        Status:    StatusOnline,
    }
    // 同步更新 Room 的成员列表
    r.addUserToRoom(userID, roomID, gatewayID)
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
| 消息队列 | Kafka（数据面消息推送 + 业务请求转发） |
| 外部接口 | HTTP + JSON（RoomClose 等控制面操作） |
| 序列化 | msgpack（内部）、JSON（外部） |
| 服务发现 | Nacos |
| 一致性哈希 | hashicorp/consistent 或自研 |
| 监控 | Prometheus + Grafana |
| 日志 | zap（结构化日志） |
| 认证 | JWT (RS256) |

### 12.1 Kafka 配置

| 配置项 | 值 | 说明 |
|--------|-----|------|
| 推送消息 Topic | im_msg_broadcast | Python → Gateway 消息推送 |
| 业务请求 Topic | im_biz_request | Gateway → Python 业务请求 |
| Partition Key（广播）| room_id | 保证同房间消息有序 |
| Partition Key（单播）| user_id | - |
| Consumer Group | gateway_id | 每个 Gateway 独立消费全部消息 |

### 12.2 Nacos 配置

| 配置项 | 值 | 说明 |
|--------|-----|------|
| Gateway 服务名 | gateway | - |
| Roomserver 服务名 | roomserver | - |
| 服务列表刷新间隔 | 5s | - |
| JWT 公钥配置 Key | jwt.public_key | RS256 公钥 |

### 12.3 JWT 配置

| 配置项 | 值 | 说明 |
|--------|-----|------|
| 签名算法 | RS256 | RSA + SHA256 |
| 公钥来源 | Nacos | 配置中心统一管理 |
| 必需 Claims | sub, exp, iat, iss | 用户ID、过期时间、签发时间、签发者 |

---

## 十三、配置参数汇总

### 13.1 时间参数

| 参数 | 值 | 说明 |
|------|-----|------|
| RECOVERY MODE 超时 | 20s | Roomserver 恢复最长等待时间 |
| Disconnected 超时 | 60s | 用户断线后保留状态时间（超时后自动 LeaveRoom） |
| 心跳间隔 | 20s | 客户端心跳间隔（建议） |
| BatchReSync 批次间隔 | 10ms | 渐进式上报批次间隔（rate limit） |
| Nacos 刷新间隔 | 5s | 服务列表刷新间隔 |

### 13.1.1 用户断连通知参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 单次超时 | 3s | 通知 Roomserver 的超时时间 |
| 重试次数 | 2 | 共 3 次尝试 |
| 重试间隔 | 500ms | 快速重试 |

### 13.1.2 RoomClose 参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 单 Gateway 超时 | 3s | 预估处理：几十ms，留足余量 |
| 重试次数 | 2 | 共 3 次尝试 |
| 重试间隔 | 500ms | 快速重试 |
| 总超时上限 | 10s | 超过则返回 partial success |

### 13.1.3 Gateway 网络分区参数

| 参数 | 值 | 说明 |
|------|-----|------|
| DEGRADED 触发阈值 | 30s | 连不上 Roomserver 超过此时间进入 DEGRADED |
| Roomserver 心跳间隔 | 5s | Gateway 向 Roomserver 发送心跳 |
| 心跳超时 | 3s | 单次心跳超时时间 |
| pendingLeaves 队列上限 | 10000 | 降级期间待补偿的 LeaveRoom 上限 |

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
| 消息分级 | 开启 | Critical/High/Normal/Low 四级，通过推送 API priority 参数指定 |
| 连接保护 | 开启 | 多维度慢连接检测与断开 |
| 消息压缩 | **延后** | 第一版暂不实现，后续根据带宽压力评估 |
| ACK 驱动更新 | 开启 | Gateway 状态由 ACK 驱动更新（无 Delta） |
| 消息重放 | **延后** | Resume 不做消息重放，预留扩展接口 |

### 13.5 第一版范围

```
┌─────────────────────────────────────────────────────────────────────────┐
│  第一版实现                                                               │
├─────────────────────────────────────────────────────────────────────────┤
│  ✓ Gateway + Roomserver 核心架构                                         │
│  ✓ WebSocket 连接管理、心跳、断线检测                                      │
│  ✓ JoinRoom / LeaveRoom / Resume 流程                                   │
│  ✓ 房间广播、单播、多播                                                   │
│  ✓ ACK 驱动状态更新                                                       │
│  ✓ 消息分级（priority 参数）                                              │
│  ✓ 背压与连接保护                                                         │
│  ✓ 心跳弱自愈                                                            │
│  ✓ Roomserver 崩溃恢复（BatchReSync）                                    │
│  ✓ 基本监控指标暴露（Prometheus）                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│  延后实现（后续版本）                                                       │
├─────────────────────────────────────────────────────────────────────────┤
│  ✗ 消息压缩（zstd）                                                       │
│    - 理由：第一版先验证核心流程，压缩增加 CPU 开销                           │
│    - 时机：带宽成为瓶颈时                                                  │
│                                                                         │
│  ✗ 扩缩容逻辑                                                            │
│    - 理由：第一版固定节点数，验证后再实现动态扩缩容                           │
│    - 时机：用户规模增长需要水平扩展时                                       │
│                                                                         │
│  ✗ 完善监控告警                                                          │
│    - 理由：第一版先暴露基础指标，告警规则逐步完善                            │
│    - 时机：系统上线后根据运维经验迭代                                       │
│                                                                         │
│  ✗ 消息重放                                                              │
│    - 理由：直播场景消息时效性强，重放价值有限                                │
│    - 时机：有明确业务需求时                                                │
│                                                                         │
│  ✗ QUIC 传输                                                             │
│    - 理由：WebSocket 已能满足需求                                         │
│    - 时机：移动网络环境优化需求强烈时                                       │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 十四、后续扩展预留

| 扩展点 | 当前方案 | 预留接口 |
|--------|---------|---------|
| QUIC 传输 | WebSocket | Transport 接口抽象 |
| 消息持久化 | 不持久化 | Message 结构预留持久化字段 |
| 多机房部署 | 单机房 | 一致性哈希可扩展为跨机房 |
| 消息加密 | TLS 传输加密 | Payload 层预留端到端加密字段 |
