package kafka

import (
	"context"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"github.com/qiminjie89/imsys/pkg/logger"
)

// ProducerConfig Kafka 生产者配置
type ProducerConfig struct {
	Brokers []string // Kafka broker 地址
	Topic   string   // 目标 topic
}

// Producer Kafka 生产者
type Producer struct {
	cfg    *ProducerConfig
	writer *kafka.Writer
}

// NewProducer 创建 Kafka 生产者
func NewProducer(cfg *ProducerConfig) *Producer {
	writer := &kafka.Writer{
		Addr:     kafka.TCP(cfg.Brokers...),
		Topic:    cfg.Topic,
		Balancer: &kafka.Hash{}, // 按 key 哈希分区
	}

	return &Producer{
		cfg:    cfg,
		writer: writer,
	}
}

// Send 发送消息
func (p *Producer) Send(ctx context.Context, key, value []byte) error {
	msg := kafka.Message{
		Key:   key,
		Value: value,
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		logger.Error("kafka send failed",
			zap.Error(err),
			zap.String("topic", p.cfg.Topic),
		)
		return err
	}

	return nil
}

// SendBatch 批量发送消息
func (p *Producer) SendBatch(ctx context.Context, messages []kafka.Message) error {
	if err := p.writer.WriteMessages(ctx, messages...); err != nil {
		logger.Error("kafka batch send failed",
			zap.Error(err),
			zap.String("topic", p.cfg.Topic),
			zap.Int("count", len(messages)),
		)
		return err
	}
	return nil
}

// Close 关闭生产者
func (p *Producer) Close() error {
	return p.writer.Close()
}
