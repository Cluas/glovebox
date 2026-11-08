package main

import (
	"encoding/binary"
	"errors"
)

// feishuFrame is a frame of Feishu's long connection, the protobuf message pbbp2.Frame of the
// official SDKs. It is the only message on the wire, so it is encoded by hand rather than with a
// protobuf library.
type feishuFrame struct {
	SeqID           uint64 // 1
	LogID           uint64 // 2
	Service         int32  // 3: service_id of the connection URL
	Method          int32  // 4: 0 control (ping, pong), 1 data (events)
	Headers         []feishuHeader
	PayloadEncoding string // 6
	PayloadType     string // 7
	Payload         []byte // 8
	LogIDNew        string // 9
}

type feishuHeader struct{ Key, Value string }

const (
	feishuControl = 0
	feishuData    = 1
)

func (f *feishuFrame) header(key string) string {
	for _, h := range f.Headers {
		if h.Key == key {
			return h.Value
		}
	}
	return ""
}

func pbVarint(b []byte, field int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3)
	return binary.AppendUvarint(b, v)
}

func pbBytes(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// marshal writes the fields in order and, like the SDK, the strings even when empty
func (f *feishuFrame) marshal() []byte {
	var b []byte
	b = pbVarint(b, 1, f.SeqID)
	b = pbVarint(b, 2, f.LogID)
	b = pbVarint(b, 3, uint64(int64(f.Service)))
	b = pbVarint(b, 4, uint64(int64(f.Method)))
	for _, h := range f.Headers {
		var hb []byte
		hb = pbBytes(hb, 1, []byte(h.Key))
		hb = pbBytes(hb, 2, []byte(h.Value))
		b = pbBytes(b, 5, hb)
	}
	b = pbBytes(b, 6, []byte(f.PayloadEncoding))
	b = pbBytes(b, 7, []byte(f.PayloadType))
	if f.Payload != nil {
		b = pbBytes(b, 8, f.Payload)
	}
	return pbBytes(b, 9, []byte(f.LogIDNew))
}

var errPB = errors.New("feishu: malformed frame")

// pbFields calls fn for each field of a protobuf message; v is the value of a varint field and
// data the contents of a length-delimited one. Other wire types are skipped.
func pbFields(b []byte, fn func(field int, v uint64, data []byte) error) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return errPB
		}
		b = b[n:]
		field, wire := int(key>>3), key&7
		switch wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return errPB
			}
			b = b[n:]
			if err := fn(field, v, nil); err != nil {
				return err
			}
		case 1:
			if len(b) < 8 {
				return errPB
			}
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return errPB
			}
			data := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := fn(field, 0, data); err != nil {
				return err
			}
		case 5:
			if len(b) < 4 {
				return errPB
			}
			b = b[4:]
		default:
			return errPB
		}
	}
	return nil
}

func unmarshalFeishuFrame(b []byte) (*feishuFrame, error) {
	f := &feishuFrame{}
	err := pbFields(b, func(field int, v uint64, data []byte) error {
		switch field {
		case 1:
			f.SeqID = v
		case 2:
			f.LogID = v
		case 3:
			f.Service = int32(v)
		case 4:
			f.Method = int32(v)
		case 5:
			var h feishuHeader
			if err := pbFields(data, func(field int, _ uint64, data []byte) error {
				switch field {
				case 1:
					h.Key = string(data)
				case 2:
					h.Value = string(data)
				}
				return nil
			}); err != nil {
				return err
			}
			f.Headers = append(f.Headers, h)
		case 6:
			f.PayloadEncoding = string(data)
		case 7:
			f.PayloadType = string(data)
		case 8:
			f.Payload = append([]byte{}, data...)
		case 9:
			f.LogIDNew = string(data)
		}
		return nil
	})
	return f, err
}
