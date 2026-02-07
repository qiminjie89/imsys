package protocol

import (
	"github.com/vmihailenco/msgpack/v5"
)

// Encode 使用 msgpack 编码
func Encode(v interface{}) ([]byte, error) {
	return msgpack.Marshal(v)
}

// Decode 使用 msgpack 解码
func Decode(data []byte, v interface{}) error {
	return msgpack.Unmarshal(data, v)
}
