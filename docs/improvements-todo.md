# IMSys 待改进事项

> 基于架构评审识别的问题，按优先级排序

---

## P0 - 上线前必须解决

### 1. 速率限制 (Rate Limiting)
- **问题**: 无任何限流机制，存在 DDoS 和恶意刷消息风险
- **位置**: Gateway 入口层
- **建议**: 添加 per-user、per-IP、per-room 的多维度限流

### 2. 关键监控指标
- **问题**: 缺少关键路径的延迟和队列指标
- **缺失指标**:
  - `gateway_message_latency_seconds` - 消息端到端延迟
  - `gateway_ack_latency_seconds` - JoinRoom ACK 响应时间
  - `gateway_send_queue_wait_time_seconds` - 队列等待时间
  - `gateway_kafka_consumer_lag` - Kafka 消费延迟
  - `roomserver_recovery_duration_seconds` - 恢复耗时
- **位置**: `pkg/metrics/metrics.go`

---

## P1 - 上线后尽快解决

### 3. 重连添加 Jitter
- **问题**: 所有 Gateway 同时重连可能引发惊群效应
- **位置**: `internal/gateway/roomserver_client.go:71-81`
- **建议**:
  ```go
  jitter := time.Duration(rand.Int63n(int64(reconnectInterval / 4)))
  reconnectInterval += jitter
  ```

### 4. 细粒度降级状态
- **问题**: 当前只有 Normal/Degraded 两种状态，无法区分 Roomserver/Kafka 各自故障
- **位置**: `internal/gateway/server.go:19-25`
- **建议**: 拆分为 RoomserverDegraded、KafkaDegraded、FullDegraded

### 5. Pending 请求过期清理
- **问题**: 长时间降级时 pendingLeaves/pendingDisconnects 无限增长
- **位置**: `internal/gateway/server.go:265-285`
- **建议**: 添加 TTL (如 5 分钟) + 去重逻辑

### 6. 自适应恢复超时
- **问题**: 固定 20s 超时，Gateway 多时不够，少时浪费
- **位置**: `internal/roomserver/recovery.go`
- **建议**: `timeout = baseTimeout + connectedGateways * perGatewayTimeout`

### 7. Recovery 竞态条件
- **问题**: `SetExpectedGateways()` 可能在某些 Gateway 已完成后才调用
- **位置**: `internal/roomserver/recovery.go:37-45`
- **建议**: 使用 sync.WaitGroup 或确保设置顺序

---

## P2 - 迭代优化

### 8. 协议版本号
- **问题**: 帧格式缺少 Magic Number 和 Version 字段，不利于协议升级
- **位置**: `internal/protocol/frame.go`

### 9. 补充错误码
- **建议**:
  - 6xxx: 限流相关 (RateLimited, TooManyRequests)
  - 7xxx: 协议相关 (UnsupportedVersion, MalformedFrame)

### 10. 发送队列背压机制
- **问题**: 队列满时仅 log，发送方无感知
- **位置**: `internal/roomserver/gateway_conn.go:141-151`

### 11. Kafka 消息重试
- **问题**: 当前 fire-and-forget，业务请求可能丢失
- **位置**: `internal/gateway/kafka_producer.go:80-100`

---

## 长期规划

### 12. 大房间优化
- 单房间 5 万+ 用户时的性能优化
- 可选方案: 消息合并、边缘缓存、房间拆分

### 13. ~~消息可靠性机制~~ ✅ 已确定方案
- ~~消息 ACK + 重发 (关键消息)~~
- ~~消息回溯 (断线重连后拉取遗漏)~~
- **已采用方案**：Seq + Gateway RingBuffer + 客户端补漏（见技术方案 5.5 节）

### 14. 运维管理接口
- 强制关闭房间
- 踢出用户
- Gateway 平滑下线
- 配置热加载

---

## 已确定的设计决策

### A. Roomserver 高可用方案

**结论**: 分片 + 无副本 + BatchReSync 恢复

| 决策项 | 结论 | 理由 |
|-------|------|------|
| 主从热备 | 不做 | 复杂度高，收益有限 |
| 单房间跨分片 | 不做 | 复杂度远超收益 |
| Snapshot | 定时写，不用于自动恢复 | 用于运维灾难恢复 |

**正常恢复流程**:
```
Roomserver 重启 → 等待 Gateway BatchReSync → 恢复服务
```

**灾难恢复流程（Gateway + Roomserver 同时挂）**:
```
运维从 Snapshot 手动恢复 → 用户重新连接 JoinRoom
```

**Snapshot 实现要点**:
- 定时写入（如每 30s）
- 存储内容：房间元数据、用户列表（用于灾难恢复参考）
- 存储位置：本地文件 或 Redis
- 不阻塞主流程（异步写入）

---

## 待讨论事项

### B. 客户端重连策略
- 断连后的重试策略
- Resume 失败后的降级策略
- 网络切换的处理

---

*创建时间: 2026-03-06*
*最后更新: 2026-03-06*
