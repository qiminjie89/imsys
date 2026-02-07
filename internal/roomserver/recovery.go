package roomserver

import (
	"sync"
	"sync/atomic"
	"time"
)

// RecoveryState 恢复状态管理
type RecoveryState struct {
	startTime  time.Time
	timeout    time.Duration
	done       chan struct{}
	doneOnce   sync.Once

	// 统计
	expectedGateways  int32
	completedGateways atomic.Int32
	receivedUsers     atomic.Int32
}

// NewRecoveryState 创建恢复状态
func NewRecoveryState(timeout time.Duration) *RecoveryState {
	return &RecoveryState{
		startTime: time.Now(),
		timeout:   timeout,
		done:      make(chan struct{}),
	}
}

// SetExpectedGateways 设置预期的 Gateway 数量
func (r *RecoveryState) SetExpectedGateways(count int) {
	atomic.StoreInt32(&r.expectedGateways, int32(count))
}

// OnGatewayComplete 当一个 Gateway 完成上报
func (r *RecoveryState) OnGatewayComplete() {
	completed := r.completedGateways.Add(1)
	expected := atomic.LoadInt32(&r.expectedGateways)

	// 如果所有 Gateway 都完成了，退出恢复模式
	if expected > 0 && completed >= expected {
		r.finish()
	}
}

// OnUsersReceived 当收到用户数据
func (r *RecoveryState) OnUsersReceived(count int) {
	r.receivedUsers.Add(int32(count))
}

// Wait 等待恢复完成
func (r *RecoveryState) Wait() {
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()

	select {
	case <-r.done:
		// 正常完成
	case <-timer.C:
		// 超时
		r.finish()
	}
}

// finish 完成恢复
func (r *RecoveryState) finish() {
	r.doneOnce.Do(func() {
		close(r.done)
	})
}

// Duration 返回恢复持续时间
func (r *RecoveryState) Duration() time.Duration {
	return time.Since(r.startTime)
}

// ReceivedUsers 返回收到的用户数
func (r *RecoveryState) ReceivedUsers() int {
	return int(r.receivedUsers.Load())
}

// CompletedGateways 返回完成的 Gateway 数
func (r *RecoveryState) CompletedGateways() int {
	return int(r.completedGateways.Load())
}
