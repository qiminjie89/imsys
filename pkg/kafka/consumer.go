// Package kafka 提供 Kafka 客户端封装
package kafka

import (
	"context"
	"sync/atomic"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"github.com/qiminjie89/imsys/pkg/logger"
)

// ConsumerConfig Kafka 消费者配置
type ConsumerConfig struct {
	Brokers       []string // Kafka broker 地址
	Topic         string   // 订阅的 topic
	ConsumerGroup string   // 消费组 ID（Gateway 使用 gateway_id）
}

// Consumer Kafka 消费者
type Consumer struct {
	cfg       *ConsumerConfig
	reader    *kafka.Reader
	connected atomic.Bool
	handler   MessageHandler
}

// MessageHandler 消息处理函数
type MessageHandler func(msg *Message) error

// Message Kafka 消息
type Message struct {
	Key       []byte
	Value     []byte
	Partition int
	Offset    int64
}

// NewConsumer 创建 Kafka 消费者
func NewConsumer(cfg *ConsumerConfig) *Consumer {
	// 验证配置
	if len(cfg.Brokers) == 0 || cfg.Topic == "" || cfg.ConsumerGroup == "" {
		logger.Warn("kafka consumer config incomplete, skipping",
			zap.Int("brokers", len(cfg.Brokers)),
			zap.String("topic", cfg.Topic),
			zap.String("group", cfg.ConsumerGroup),
		)
		return nil
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  cfg.Brokers,
		Topic:    cfg.Topic,
		GroupID:  cfg.ConsumerGroup,
		MinBytes: 1,
		MaxBytes: 10e6, // 10MB
	})

	c := &Consumer{
		cfg:    cfg,
		reader: reader,
	}
	c.connected.Store(true)

	return c
}

// Start 启动消费循环
func (c *Consumer) Start(ctx context.Context, handler MessageHandler) {
	c.handler = handler

	logger.Info("kafka consumer started",
		zap.String("topic", c.cfg.Topic),
		zap.String("group", c.cfg.ConsumerGroup),
	)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			msg, err := c.reader.FetchMessage(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				c.connected.Store(false)
				logger.Error("kafka fetch message failed", zap.Error(err))
				continue
			}

			c.connected.Store(true)

			if err := handler(&Message{
				Key:       msg.Key,
				Value:     msg.Value,
				Partition: msg.Partition,
				Offset:    msg.Offset,
			}); err != nil {
				logger.Error("kafka message handler failed",
					zap.Error(err),
					zap.Int("partition", msg.Partition),
					zap.Int64("offset", msg.Offset),
				)
			}

			// 提交 offset
			if err := c.reader.CommitMessages(ctx, msg); err != nil {
				logger.Error("kafka commit failed", zap.Error(err))
			}
		}
	}
}

// IsConnected 检查是否连接正常
func (c *Consumer) IsConnected() bool {
	return c.connected.Load()
}

// Close 关闭消费者
func (c *Consumer) Close() error {
	return c.reader.Close()
}
