package gateway

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/kafka"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
)

// PushMessage Kafka 推送消息格式
type PushMessage struct {
	Scope    string   `json:"scope"`     // room | user | platform | multicast
	RoomID   string   `json:"room_id"`   // scope=room/multicast 时必填
	UserID   string   `json:"user_id"`   // scope=user 时必填
	UserIDs  []string `json:"user_ids"`  // scope=multicast 时必填
	Seq      uint64   `json:"seq"`       // Python 生成，用于保序
	Priority string   `json:"priority"`  // critical/high/normal/low
	Message  json.RawMessage `json:"message"` // 业务消息内容
}

// startKafkaConsumer 启动 Kafka 消费者（数据面）
func (s *Server) startKafkaConsumer(ctx context.Context) {
	// 如果没有配置 Kafka，跳过
	if len(s.cfg.Kafka.Brokers) == 0 || s.cfg.Kafka.PushTopic == "" {
		logger.Warn("kafka consumer disabled (no brokers or topic configured)")
		return
	}

	cfg := &kafka.ConsumerConfig{
		Brokers:       s.cfg.Kafka.Brokers,
		Topic:         s.cfg.Kafka.PushTopic,
		ConsumerGroup: s.cfg.Gateway.ID, // 每个 Gateway 独立消费组
	}

	consumer := kafka.NewConsumer(cfg)
	if consumer == nil {
		logger.Warn("failed to create kafka consumer")
		return
	}
	s.kafkaConsumer = consumer

	logger.Info("starting kafka consumer for data plane",
		zap.String("topic", cfg.Topic),
		zap.String("consumer_group", cfg.ConsumerGroup),
	)

	consumer.Start(ctx, s.handlePushMessage)
}

// handlePushMessage 处理 Kafka 推送消息
func (s *Server) handlePushMessage(msg *kafka.Message) error {
	var pushMsg PushMessage
	if err := json.Unmarshal(msg.Value, &pushMsg); err != nil {
		logger.Error("failed to unmarshal push message", zap.Error(err))
		metrics.GatewayKafkaMessageDropped.Inc()
		return nil // 不重试，丢弃
	}

	metrics.GatewayKafkaMessageReceived.Inc()

	switch pushMsg.Scope {
	case "room":
		s.handleRoomBroadcast(&pushMsg)
	case "user":
		s.handleUnicast(&pushMsg)
	case "multicast":
		s.handleMulticast(&pushMsg)
	case "platform":
		s.handlePlatformBroadcast(&pushMsg)
	default:
		logger.Warn("unknown push scope", zap.String("scope", pushMsg.Scope))
	}

	return nil
}

// handleRoomBroadcast 处理房间广播
func (s *Server) handleRoomBroadcast(msg *PushMessage) {
	s.localRoomsMu.RLock()
	users, ok := s.localRooms[msg.RoomID]
	s.localRoomsMu.RUnlock()

	if !ok {
		// 该房间不在本 Gateway，丢弃
		metrics.GatewayKafkaMessageFiltered.Inc()
		return
	}

	// 构建推送消息
	payload := buildPushPayload(msg)

	// 分发到各 Distributor
	for userID := range users {
		s.dispatchToUser(userID, payload, msg.Priority)
	}

	metrics.GatewayBroadcastMessages.Inc()
}

// handleUnicast 处理单播
func (s *Server) handleUnicast(msg *PushMessage) {
	s.localUsersMu.RLock()
	_, ok := s.localUsers[msg.UserID]
	s.localUsersMu.RUnlock()

	if !ok {
		// 该用户不在本 Gateway，丢弃
		metrics.GatewayKafkaMessageFiltered.Inc()
		return
	}

	payload := buildPushPayload(msg)
	s.dispatchToUser(msg.UserID, payload, msg.Priority)

	metrics.GatewayUnicastMessages.Inc()
}

// handleMulticast 处理多播
func (s *Server) handleMulticast(msg *PushMessage) {
	s.localRoomsMu.RLock()
	_, roomExists := s.localRooms[msg.RoomID]
	s.localRoomsMu.RUnlock()

	if !roomExists {
		metrics.GatewayKafkaMessageFiltered.Inc()
		return
	}

	payload := buildPushPayload(msg)

	s.localUsersMu.RLock()
	for _, userID := range msg.UserIDs {
		if _, ok := s.localUsers[userID]; ok {
			s.dispatchToUser(userID, payload, msg.Priority)
		}
	}
	s.localUsersMu.RUnlock()

	metrics.GatewayMulticastMessages.Inc()
}

// handlePlatformBroadcast 处理平台广播
func (s *Server) handlePlatformBroadcast(msg *PushMessage) {
	payload := buildPushPayload(msg)

	// 遍历所有本地房间的用户
	s.localRoomsMu.RLock()
	for _, users := range s.localRooms {
		for userID := range users {
			s.dispatchToUser(userID, payload, msg.Priority)
		}
	}
	s.localRoomsMu.RUnlock()

	metrics.GatewayPlatformBroadcastMessages.Inc()
}

// dispatchToUser 分发消息到用户
func (s *Server) dispatchToUser(userID string, payload []byte, priority string) {
	s.connsMu.RLock()
	conn, ok := s.connections[userID]
	s.connsMu.RUnlock()

	if !ok {
		return
	}

	// 获取优先级并检查是否应该丢弃
	msgPriority := parsePriority(priority)
	queueUsage := conn.QueueUsage()

	// 根据队列状态和消息优先级决定是否投递
	shouldDrop := false
	switch {
	case queueUsage >= 0.95:
		shouldDrop = msgPriority > PriorityCritical
	case queueUsage >= 0.80:
		shouldDrop = msgPriority > PriorityHigh
	case queueUsage >= 0.50:
		shouldDrop = msgPriority > PriorityNormal
	}

	if shouldDrop {
		return
	}

	// 发送到连接的 sendCh
	conn.Send(payload)
}

// buildPushPayload 构建推送消息的 payload（带帧头）
func buildPushPayload(msg *PushMessage) []byte {
	// 构建推送内容
	pushContent := struct {
		Scope   string          `msgpack:"scope"`
		RoomID  string          `msgpack:"room_id,omitempty"`
		UserID  string          `msgpack:"user_id,omitempty"`
		Seq     uint64          `msgpack:"seq"`
		Message json.RawMessage `msgpack:"message"`
	}{
		Scope:   msg.Scope,
		RoomID:  msg.RoomID,
		UserID:  msg.UserID,
		Seq:     msg.Seq,
		Message: msg.Message,
	}

	payload, _ := protocol.Encode(pushContent)

	// 构建完整帧
	frame := &protocol.Frame{
		MsgType: protocol.MsgTypePushMessage,
		Seq:     msg.Seq,
		Payload: payload,
	}

	return protocol.EncodeFrame(frame)
}

// parsePriority 解析优先级字符串
func parsePriority(priority string) MessagePriority {
	switch priority {
	case "critical":
		return PriorityCritical
	case "high":
		return PriorityHigh
	case "low":
		return PriorityLow
	default:
		return PriorityNormal
	}
}

// MessagePriority 消息优先级
type MessagePriority int

const (
	PriorityCritical MessagePriority = 0
	PriorityHigh     MessagePriority = 1
	PriorityNormal   MessagePriority = 2
	PriorityLow      MessagePriority = 3
)
