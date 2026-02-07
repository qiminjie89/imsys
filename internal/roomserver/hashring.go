package roomserver

import (
	"hash/crc32"
	"sort"
	"strconv"
	"sync"
)

// HashRing 一致性哈希环
type HashRing struct {
	virtualNodes int
	ring         []uint32          // 排序的哈希值
	nodes        map[uint32]string // 哈希值 → 节点 ID
	mu           sync.RWMutex
}

// NewHashRing 创建哈希环
func NewHashRing(virtualNodes int) *HashRing {
	return &HashRing{
		virtualNodes: virtualNodes,
		nodes:        make(map[uint32]string),
	}
}

// AddNode 添加节点
func (h *HashRing) AddNode(nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i := 0; i < h.virtualNodes; i++ {
		key := nodeID + "#" + strconv.Itoa(i)
		hash := h.hash(key)
		h.ring = append(h.ring, hash)
		h.nodes[hash] = nodeID
	}

	sort.Slice(h.ring, func(i, j int) bool {
		return h.ring[i] < h.ring[j]
	})
}

// RemoveNode 移除节点
func (h *HashRing) RemoveNode(nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	newRing := make([]uint32, 0, len(h.ring)-h.virtualNodes)
	for _, hash := range h.ring {
		if h.nodes[hash] != nodeID {
			newRing = append(newRing, hash)
		} else {
			delete(h.nodes, hash)
		}
	}
	h.ring = newRing
}

// GetNode 获取负责该 key 的节点
func (h *HashRing) GetNode(key string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.ring) == 0 {
		return ""
	}

	hash := h.hash(key)
	idx := sort.Search(len(h.ring), func(i int) bool {
		return h.ring[i] >= hash
	})

	if idx >= len(h.ring) {
		idx = 0
	}

	return h.nodes[h.ring[idx]]
}

// hash 计算哈希值
func (h *HashRing) hash(key string) uint32 {
	return crc32.ChecksumIEEE([]byte(key))
}

// IsLocalNode 判断 key 是否由本节点负责
func (h *HashRing) IsLocalNode(key, localNodeID string) bool {
	return h.GetNode(key) == localNodeID
}
