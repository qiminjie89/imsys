// Package nacos 提供 Nacos 服务发现和配置管理
package nacos

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/qiminjie89/imsys/pkg/logger"
)

// Config Nacos 客户端配置
type Config struct {
	ServerAddr string // Nacos 服务地址
	Namespace  string // 命名空间
	Group      string // 分组
}

// ServiceInstance 服务实例
type ServiceInstance struct {
	ServiceName string
	IP          string
	Port        int
	Metadata    map[string]string
	Healthy     bool
	Weight      float64
}

// Address 返回服务地址
func (s *ServiceInstance) Address() string {
	return fmt.Sprintf("%s:%d", s.IP, s.Port)
}

// Client Nacos 客户端
type Client struct {
	cfg       *Config
	mu        sync.RWMutex
	services  map[string][]*ServiceInstance // serviceName → instances
	callbacks map[string][]ServiceCallback  // serviceName → callbacks
	running   bool
	stopCh    chan struct{}
}

// ServiceCallback 服务变更回调
type ServiceCallback func(instances []*ServiceInstance)

// NewClient 创建 Nacos 客户端
func NewClient(cfg *Config) *Client {
	c := &Client{
		cfg:       cfg,
		services:  make(map[string][]*ServiceInstance),
		callbacks: make(map[string][]ServiceCallback),
		stopCh:    make(chan struct{}),
	}

	// 启动后台刷新（模拟 Nacos 心跳）
	go c.backgroundRefresh()

	return c
}

// backgroundRefresh 后台刷新服务列表（模拟实现）
func (c *Client) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.refreshServices()
		}
	}
}

// refreshServices 刷新服务列表
func (c *Client) refreshServices() {
	c.mu.RLock()
	serviceNames := make([]string, 0, len(c.callbacks))
	for name := range c.callbacks {
		serviceNames = append(serviceNames, name)
	}
	c.mu.RUnlock()

	for _, name := range serviceNames {
		instances := c.fetchInstances(name)
		c.updateInstances(name, instances)
	}
}

// fetchInstances 获取服务实例（这里是模拟实现）
func (c *Client) fetchInstances(serviceName string) []*ServiceInstance {
	// TODO: 实际实现应使用 nacos-sdk-go 调用 Nacos API
	// 这里返回模拟数据用于开发测试

	// 默认返回本地 Roomserver 实例
	if serviceName == "roomserver" {
		return []*ServiceInstance{
			{
				ServiceName: "roomserver",
				IP:          "localhost",
				Port:        9000,
				Healthy:     true,
				Weight:      1.0,
			},
		}
	}

	return nil
}

// updateInstances 更新服务实例并触发回调
func (c *Client) updateInstances(serviceName string, instances []*ServiceInstance) {
	c.mu.Lock()
	oldInstances := c.services[serviceName]
	c.services[serviceName] = instances
	callbacks := c.callbacks[serviceName]
	c.mu.Unlock()

	// 检查是否有变更
	if !instancesEqual(oldInstances, instances) {
		logger.Info("service instances changed",
			zap.String("service", serviceName),
			zap.Int("count", len(instances)),
		)

		// 触发回调
		for _, cb := range callbacks {
			go cb(instances)
		}
	}
}

// instancesEqual 比较两个实例列表是否相等
func instancesEqual(a, b []*ServiceInstance) bool {
	if len(a) != len(b) {
		return false
	}
	aMap := make(map[string]bool)
	for _, inst := range a {
		aMap[inst.Address()] = inst.Healthy
	}
	for _, inst := range b {
		if healthy, ok := aMap[inst.Address()]; !ok || healthy != inst.Healthy {
			return false
		}
	}
	return true
}

// RegisterService 注册服务
func (c *Client) RegisterService(instance *ServiceInstance) error {
	logger.Info("registering service to nacos",
		zap.String("service", instance.ServiceName),
		zap.String("ip", instance.IP),
		zap.Int("port", instance.Port),
	)

	// TODO: 实现实际的 Nacos 注册逻辑
	// 使用 nacos-sdk-go

	return nil
}

// DeregisterService 注销服务
func (c *Client) DeregisterService(instance *ServiceInstance) error {
	logger.Info("deregistering service from nacos",
		zap.String("service", instance.ServiceName),
	)

	// TODO: 实现实际的 Nacos 注销逻辑

	return nil
}

// Subscribe 订阅服务变更
func (c *Client) Subscribe(serviceName string, callback ServiceCallback) error {
	c.mu.Lock()
	c.callbacks[serviceName] = append(c.callbacks[serviceName], callback)
	c.mu.Unlock()

	logger.Info("subscribed to service",
		zap.String("service", serviceName),
	)

	// 立即获取一次
	instances := c.fetchInstances(serviceName)
	c.updateInstances(serviceName, instances)

	return nil
}

// GetInstances 获取服务实例列表
func (c *Client) GetInstances(serviceName string) []*ServiceInstance {
	c.mu.RLock()
	instances := c.services[serviceName]
	c.mu.RUnlock()

	// 如果没有缓存，立即获取
	if len(instances) == 0 {
		instances = c.fetchInstances(serviceName)
		if len(instances) > 0 {
			c.mu.Lock()
			c.services[serviceName] = instances
			c.mu.Unlock()
		}
	}

	return instances
}

// GetHealthyInstance 获取一个健康的服务实例（带负载均衡）
func (c *Client) GetHealthyInstance(serviceName string) *ServiceInstance {
	instances := c.GetInstances(serviceName)
	if len(instances) == 0 {
		return nil
	}

	// 过滤健康实例
	healthy := make([]*ServiceInstance, 0, len(instances))
	for _, inst := range instances {
		if inst.Healthy {
			healthy = append(healthy, inst)
		}
	}

	if len(healthy) == 0 {
		return nil
	}

	// 简单的随机负载均衡
	return healthy[rand.Intn(len(healthy))]
}

// GetConfig 获取配置
func (c *Client) GetConfig(dataID string) (string, error) {
	logger.Debug("getting config from nacos",
		zap.String("dataID", dataID),
	)

	// TODO: 实现实际的 Nacos 配置获取逻辑

	return "", nil
}

// WatchConfig 监听配置变更
func (c *Client) WatchConfig(dataID string, callback func(content string)) error {
	logger.Info("watching config",
		zap.String("dataID", dataID),
	)

	// TODO: 实现实际的 Nacos 配置监听逻辑

	return nil
}

// Close 关闭客户端
func (c *Client) Close() error {
	logger.Info("closing nacos client")
	close(c.stopCh)
	return nil
}
