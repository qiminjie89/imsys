package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

/*
WebSocket 消息帧格式：
+----------+----------+----------+------------------+
|  MsgType |   Seq    |  Length  |     Payload      |
|  4 bytes |  8 bytes |  4 bytes |   变长 (msgpack)  |
+----------+----------+----------+------------------+
*/

const (
	HeaderSize    = 16 // 4 + 8 + 4
	MaxPayloadLen = 1 << 20 // 1MB
)

var (
	ErrPayloadTooLarge = errors.New("payload too large")
	ErrInvalidFrame    = errors.New("invalid frame")
)

// Frame 表示一个消息帧
type Frame struct {
	MsgType uint32
	Seq     uint64
	Payload []byte
}

// EncodeFrame 编码消息帧
func EncodeFrame(f *Frame) []byte {
	payloadLen := len(f.Payload)
	buf := make([]byte, HeaderSize+payloadLen)

	binary.BigEndian.PutUint32(buf[0:4], f.MsgType)
	binary.BigEndian.PutUint64(buf[4:12], f.Seq)
	binary.BigEndian.PutUint32(buf[12:16], uint32(payloadLen))

	if payloadLen > 0 {
		copy(buf[HeaderSize:], f.Payload)
	}

	return buf
}

// DecodeFrame 解码消息帧
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, ErrInvalidFrame
	}

	msgType := binary.BigEndian.Uint32(data[0:4])
	seq := binary.BigEndian.Uint64(data[4:12])
	payloadLen := binary.BigEndian.Uint32(data[12:16])

	if payloadLen > MaxPayloadLen {
		return nil, ErrPayloadTooLarge
	}

	expectedLen := HeaderSize + int(payloadLen)
	if len(data) < expectedLen {
		return nil, ErrInvalidFrame
	}

	var payload []byte
	if payloadLen > 0 {
		payload = make([]byte, payloadLen)
		copy(payload, data[HeaderSize:expectedLen])
	}

	return &Frame{
		MsgType: msgType,
		Seq:     seq,
		Payload: payload,
	}, nil
}

// ReadFrame 从 reader 读取一个帧
func ReadFrame(r io.Reader) (*Frame, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	msgType := binary.BigEndian.Uint32(header[0:4])
	seq := binary.BigEndian.Uint64(header[4:12])
	payloadLen := binary.BigEndian.Uint32(header[12:16])

	if payloadLen > MaxPayloadLen {
		return nil, ErrPayloadTooLarge
	}

	var payload []byte
	if payloadLen > 0 {
		payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}

	return &Frame{
		MsgType: msgType,
		Seq:     seq,
		Payload: payload,
	}, nil
}

// WriteFrame 写入一个帧到 writer
func WriteFrame(w io.Writer, f *Frame) error {
	data := EncodeFrame(f)
	_, err := w.Write(data)
	return err
}
