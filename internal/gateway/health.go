package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// HealthStatus 健康状态
type HealthStatus struct {
	Status              string  `json:"status"`
	Reason              string  `json:"reason,omitempty"`
	Connections         int     `json:"connections"`
	RoomserverConnected bool    `json:"roomserver_connected"`
	KafkaConnected      bool    `json:"kafka_connected"`
	UptimeSeconds       float64 `json:"uptime_seconds"`
}

var startTime = time.Now()

// runHealthServer 运行健康检查服务
func (s *Server) runHealthServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.healthHandler)

	server := &http.Server{
		Addr:    s.cfg.Server.HealthAddr,
		Handler: mux,
	}

	logger.Info("starting health server",
		zap.String("addr", s.cfg.Server.HealthAddr),
	)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("health server error", zap.Error(err))
	}
}

// healthHandler 健康检查处理
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	s.connMu.RLock()
	connCount := len(s.connections)
	s.connMu.RUnlock()

	health := &HealthStatus{
		Connections:         connCount,
		RoomserverConnected: s.roomserverClient != nil && s.roomserverClient.IsConnected(),
		KafkaConnected:      s.kafkaProducer != nil && s.kafkaProducer.IsConnected(),
		UptimeSeconds:       time.Since(startTime).Seconds(),
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}
