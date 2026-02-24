// Package metrics 提供 Prometheus 监控指标
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Gateway 指标
var (
	// 连接指标
	GatewayConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_connections_total",
		Help: "Total number of active connections",
	})

	GatewayConnectionsInRoom = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_connections_in_room",
		Help: "Number of connections per room",
	}, []string{"room_id"})

	// 消息指标
	GatewayMessagesReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_messages_received_total",
		Help: "Total messages received from clients",
	}, []string{"msg_type"})

	GatewayMessagesSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_messages_sent_total",
		Help: "Total messages sent to clients",
	}, []string{"msg_type"})

	GatewayMessagesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_messages_dropped_total",
		Help: "Total messages dropped due to backpressure",
	}, []string{"priority"})

	// 队列指标
	GatewayQueueUsage = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_connection_queue_usage",
		Help:    "Connection send queue usage ratio",
		Buckets: []float64{0.1, 0.25, 0.5, 0.75, 0.8, 0.9, 0.95, 1.0},
	})

	// 连接关闭原因
	GatewayConnectionCloseReason = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_connection_close_total",
		Help: "Connection close count by reason",
	}, []string{"reason"})

	// 写超时
	GatewayWriteTimeouts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_write_timeouts_total",
		Help: "Total write timeout count",
	})

	// Distributor 指标
	DistributorQueueSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_distributor_queue_size",
		Help: "Distributor input queue size",
	}, []string{"shard"})

	// Kafka 消费指标（数据面）
	GatewayKafkaMessageReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_kafka_messages_received_total",
		Help: "Total messages received from Kafka",
	})

	GatewayKafkaMessageFiltered = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_kafka_messages_filtered_total",
		Help: "Total messages filtered (not for this gateway)",
	})

	GatewayKafkaMessageDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_kafka_messages_dropped_total",
		Help: "Total messages dropped due to parse error",
	})

	// 消息类型统计
	GatewayBroadcastMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_broadcast_messages_total",
		Help: "Total room broadcast messages processed",
	})

	GatewayUnicastMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_unicast_messages_total",
		Help: "Total unicast messages processed",
	})

	GatewayMulticastMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_multicast_messages_total",
		Help: "Total multicast messages processed",
	})

	GatewayPlatformBroadcastMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_platform_broadcast_messages_total",
		Help: "Total platform broadcast messages processed",
	})
)

// Roomserver 指标
var (
	// 房间指标
	RoomserverRooms = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "roomserver_rooms_total",
		Help: "Total number of active rooms",
	})

	RoomserverRoomMembers = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "roomserver_room_members",
		Help:    "Room member count distribution",
		Buckets: []float64{10, 100, 500, 1000, 5000, 10000, 50000},
	})

	// 用户指标
	RoomserverUsers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "roomserver_users_total",
		Help: "Total number of users",
	})

	// 消息指标
	RoomserverBroadcasts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "roomserver_broadcasts_total",
		Help: "Total broadcast messages",
	}, []string{"type"}) // room, multicast, unicast

	// Gateway 连接
	RoomserverGatewayConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "roomserver_gateway_connections",
		Help: "Number of connected gateways",
	})

	// 恢复模式
	RoomserverRecoveryMode = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "roomserver_recovery_mode",
		Help: "1 if in recovery mode, 0 otherwise",
	})

	RoomserverEpoch = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "roomserver_epoch",
		Help: "Current epoch number",
	})

	// 请求延迟
	RoomserverRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "roomserver_request_duration_seconds",
		Help:    "Request processing duration",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})
)
