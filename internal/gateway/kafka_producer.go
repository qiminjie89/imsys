package gateway

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// KafkaProducer Kafka 生产者
type KafkaProducer struct {
	writer    *kafka.Writer
	connected atomic.Bool
}

// BizMessage 业务消息
type BizMessage struct {
	MsgID   string `json:"msg_id"`
	UserID  string `json:"user_id"`
	RoomID  string `json:"room_id"`
	Type    string `json:"type"`
	Payload []byte `json:"data"`
	Seq     uint64 `json:"seq"`
	Time    int64  `json:"timestamp"`
}

// NewKafkaProducer 创建 Kafka 生产者
func NewKafkaProducer(cfg *config.KafkaConfig) (*KafkaProducer, error) {
	writer := &kafka.Writer{
		Addr:         kafka.TCP(cfg.Brokers...),
		Topic:        cfg.Topic,
		Balancer:     &kafka.LeastBytes{},
		BatchSize:    cfg.BatchSize,
		BatchTimeout: cfg.BatchTimeout,
		Async:        true, // 异步发送
	}

	p := &KafkaProducer{
		writer: writer,
	}

	// 检查连接
	go p.checkConnection()

	return p, nil
}

// checkConnection 检查连接状态
func (p *KafkaProducer) checkConnection() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := p.writer.WriteMessages(ctx, kafka.Message{
			Key:   []byte("health-check"),
			Value: []byte("ping"),
		})
		cancel()

		if err != nil {
			p.connected.Store(false)
			logger.Warn("kafka connection check failed", zap.Error(err))
		} else {
			p.connected.Store(true)
		}

		time.Sleep(30 * time.Second)
	}
}

// Send 发送消息
func (p *KafkaProducer) Send(msg *BizMessage) error {
	msg.Time = time.Now().UnixMilli()
	msg.MsgID = generateMsgID()

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	err = p.writer.WriteMessages(context.Background(), kafka.Message{
		Key:   []byte(msg.UserID),
		Value: data,
	})

	if err != nil {
		logger.Warn("kafka send failed",
			zap.String("user_id", msg.UserID),
			zap.Error(err),
		)
	}

	return err
}

// IsConnected 返回是否已连接
func (p *KafkaProducer) IsConnected() bool {
	return p.connected.Load()
}

// Close 关闭生产者
func (p *KafkaProducer) Close() error {
	if p.writer != nil {
		return p.writer.Close()
	}
	return nil
}

// generateMsgID 生成消息 ID
func generateMsgID() string {
	// 简单实现，生产环境应使用 UUID 或雪花算法
	return time.Now().Format("20060102150405.000000")
}
