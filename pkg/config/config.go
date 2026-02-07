// Package config 提供配置加载功能
package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// GatewayConfig Gateway 配置
type GatewayConfig struct {
	Server      ServerConfig      `yaml:"server"`
	WebSocket   WebSocketConfig   `yaml:"websocket"`
	Distributor DistributorConfig `yaml:"distributor"`
	Connection  ConnectionConfig  `yaml:"connection"`
	Protection  ProtectionConfig  `yaml:"protection"`
	Roomserver  RoomserverClient  `yaml:"roomserver"`
	Kafka       KafkaConfig       `yaml:"kafka"`
	Nacos       NacosConfig       `yaml:"nacos"`
	Log         LogConfig         `yaml:"log"`
	Metrics     MetricsConfig     `yaml:"metrics"`
}

// RoomserverConfig Roomserver 配置
type RoomserverConfig struct {
	Server   RoomserverServerConfig `yaml:"server"`
	Room     RoomConfig             `yaml:"room"`
	Recovery RecoveryConfig         `yaml:"recovery"`
	HashRing HashRingConfig         `yaml:"hashring"`
	Nacos    NacosConfig            `yaml:"nacos"`
	Log      LogConfig              `yaml:"log"`
	Metrics  MetricsConfig          `yaml:"metrics"`
}

// ServerConfig 服务器基础配置
type ServerConfig struct {
	ID         string `yaml:"id"`
	Addr       string `yaml:"addr"`
	HealthAddr string `yaml:"health_addr"`
}

// RoomserverServerConfig Roomserver 服务器配置
type RoomserverServerConfig struct {
	ID       string `yaml:"id"`
	GRPCAddr string `yaml:"grpc_addr"`
	HTTPAddr string `yaml:"http_addr"`
}

// WebSocketConfig WebSocket 配置
type WebSocketConfig struct {
	ReadBufferSize   int           `yaml:"read_buffer_size"`
	WriteBufferSize  int           `yaml:"write_buffer_size"`
	HandshakeTimeout time.Duration `yaml:"handshake_timeout"`
}

// DistributorConfig Distributor 配置
type DistributorConfig struct {
	Shards         int `yaml:"shards"`
	InputQueueSize int `yaml:"input_queue_size"`
}

// ConnectionConfig 连接配置
type ConnectionConfig struct {
	SendChSize        int           `yaml:"send_ch_size"`
	BatchInterval     time.Duration `yaml:"batch_interval"`
	BatchMaxSize      int           `yaml:"batch_max_size"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
}

// ProtectionConfig 连接保护配置
type ProtectionConfig struct {
	QueueWarningRatio            float64       `yaml:"queue_warning_ratio"`
	QueueSevereRatio             float64       `yaml:"queue_severe_ratio"`
	QueueCriticalRatio           float64       `yaml:"queue_critical_ratio"`
	QueueFullTimeout             time.Duration `yaml:"queue_full_timeout"`
	MaxConsecutiveDrops          int           `yaml:"max_consecutive_drops"`
	WriteTimeout                 time.Duration `yaml:"write_timeout"`
	MaxConsecutiveWriteTimeouts  int           `yaml:"max_consecutive_write_timeouts"`
}

// RoomserverClient Roomserver 客户端配置
type RoomserverClient struct {
	ReconnectInterval    time.Duration `yaml:"reconnect_interval"`
	ReconnectMaxInterval time.Duration `yaml:"reconnect_max_interval"`
}

// RoomConfig Room 配置
type RoomConfig struct {
	MsgChSize               int           `yaml:"msg_ch_size"`
	MemberDisconnectTimeout time.Duration `yaml:"member_disconnect_timeout"`
}

// RecoveryConfig 恢复模式配置
type RecoveryConfig struct {
	Timeout                 time.Duration `yaml:"timeout"`
	PushQueueSize           int           `yaml:"push_queue_size"`
	GatewayRequestQueueSize int           `yaml:"gateway_request_queue_size"`
	ReSyncBatchSize         int           `yaml:"resync_batch_size"`
	ReSyncBatchInterval     time.Duration `yaml:"resync_batch_interval"`
}

// HashRingConfig 一致性哈希配置
type HashRingConfig struct {
	VirtualNodes int `yaml:"virtual_nodes"`
}

// KafkaConfig Kafka 配置
type KafkaConfig struct {
	Brokers      []string      `yaml:"brokers"`
	Topic        string        `yaml:"topic"`
	BatchSize    int           `yaml:"batch_size"`
	BatchTimeout time.Duration `yaml:"batch_timeout"`
}

// NacosConfig Nacos 配置
type NacosConfig struct {
	ServerAddr         string `yaml:"server_addr"`
	Namespace          string `yaml:"namespace"`
	ServiceName        string `yaml:"service_name"`
	RoomserverService  string `yaml:"roomserver_service,omitempty"`
	GatewayService     string `yaml:"gateway_service,omitempty"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	Output string `yaml:"output"`
}

// MetricsConfig 监控配置
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// LoadGatewayConfig 加载 Gateway 配置
func LoadGatewayConfig(path string) (*GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg GatewayConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// LoadRoomserverConfig 加载 Roomserver 配置
func LoadRoomserverConfig(path string) (*RoomserverConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg RoomserverConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
