package msgcache

import (
	"encoding/binary"
	"errors"

	imaplib "github.com/emersion/go-imap/v2"
)

// cacheFieldBodyStructure is the cache field name for the encoded structure.
const cacheFieldBodyStructure = "yarilo.bodystructure"

// bsCodecVersion is the first byte of every encoded body structure.
const bsCodecVersion = 1

const (
	bsKindSingle byte = 1
	bsKindMulti  byte = 2
)

func putU32(b []byte, v uint32) []byte {
	var x [4]byte
	binary.LittleEndian.PutUint32(x[:], v)
	return append(b, x[:]...)
}

func putI64(b []byte, v int64) []byte {
	var x [8]byte
	binary.LittleEndian.PutUint64(x[:], uint64(v))
	return append(b, x[:]...)
}

func getU32(b []byte) (uint32, []byte, error) {
	if len(b) < 4 {
		return 0, nil, errors.New("short u32")
	}
	return binary.LittleEndian.Uint32(b), b[4:], nil
}

func getI64(b []byte) (int64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, errors.New("short i64")
	}
	return int64(binary.LittleEndian.Uint64(b)), b[8:], nil
}

func putParams(b []byte, m map[string]string) []byte {
	b = putU32(b, uint32(len(m)))
	// Deterministic bytes are not required (the value is content-addressed by
	// its offset, never compared), so map order is fine.
	for k, v := range m {
		b = putStr(b, k)
		b = putStr(b, v)
	}
	return b
}

func getParams(b []byte) (map[string]string, []byte, error) {
	n, b, err := getU32(b)
	if err != nil || n > 1<<12 {
		return nil, nil, errors.New("params count")
	}
	if n == 0 {
		return nil, b, nil
	}
	m := make(map[string]string, n)
	for i := uint32(0); i < n; i++ {
		var k, v string
		if k, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if v, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		m[k] = v
	}
	return m, b, nil
}

func putStrs(b []byte, ss []string) []byte {
	b = putU32(b, uint32(len(ss)))
	for _, s := range ss {
		b = putStr(b, s)
	}
	return b
}

func getStrs(b []byte) ([]string, []byte, error) {
	n, b, err := getU32(b)
	if err != nil || n > 1<<12 {
		return nil, nil, errors.New("strs count")
	}
	var out []string
	for i := uint32(0); i < n; i++ {
		var s string
		if s, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		out = append(out, s)
	}
	return out, b, nil
}

func putDisposition(b []byte, d *imaplib.BodyStructureDisposition) []byte {
	if d == nil {
		return append(b, 0)
	}
	b = append(b, 1)
	b = putStr(b, d.Value)
	return putParams(b, d.Params)
}

func getDisposition(b []byte) (*imaplib.BodyStructureDisposition, []byte, error) {
	if len(b) < 1 {
		return nil, nil, errors.New("short disposition")
	}
	present := b[0]
	b = b[1:]
	if present == 0 {
		return nil, b, nil
	}
	d := &imaplib.BodyStructureDisposition{}
	var err error
	if d.Value, b, err = getStr(b); err != nil {
		return nil, nil, err
	}
	if d.Params, b, err = getParams(b); err != nil {
		return nil, nil, err
	}
	return d, b, nil
}

func encodeBSNode(b []byte, bs imaplib.BodyStructure) []byte {
	switch p := bs.(type) {
	case *imaplib.BodyStructureSinglePart:
		b = append(b, bsKindSingle)
		b = putStr(b, p.Type)
		b = putStr(b, p.Subtype)
		b = putParams(b, p.Params)
		b = putStr(b, p.ID)
		b = putStr(b, p.Description)
		b = putStr(b, p.Encoding)
		b = putU32(b, p.Size)
		if p.MessageRFC822 != nil {
			b = append(b, 1)
			env := []byte{}
			if p.MessageRFC822.Envelope != nil {
				env = encodeEnvelope(p.MessageRFC822.Envelope)
			}
			b = putU32(b, uint32(len(env)))
			b = append(b, env...)
			if p.MessageRFC822.BodyStructure != nil {
				b = append(b, 1)
				b = encodeBSNode(b, p.MessageRFC822.BodyStructure)
			} else {
				b = append(b, 0)
			}
			b = putI64(b, p.MessageRFC822.NumLines)
		} else {
			b = append(b, 0)
		}
		if p.Text != nil {
			b = append(b, 1)
			b = putI64(b, p.Text.NumLines)
		} else {
			b = append(b, 0)
		}
		if p.Extended != nil {
			b = append(b, 1)
			b = putDisposition(b, p.Extended.Disposition)
			b = putStrs(b, p.Extended.Language)
			b = putStr(b, p.Extended.Location)
		} else {
			b = append(b, 0)
		}
		return b
	case *imaplib.BodyStructureMultiPart:
		b = append(b, bsKindMulti)
		b = putStr(b, p.Subtype)
		b = putU32(b, uint32(len(p.Children)))
		for _, c := range p.Children {
			b = encodeBSNode(b, c)
		}
		if p.Extended != nil {
			b = append(b, 1)
			b = putParams(b, p.Extended.Params)
			b = putDisposition(b, p.Extended.Disposition)
			b = putStrs(b, p.Extended.Language)
			b = putStr(b, p.Extended.Location)
		} else {
			b = append(b, 0)
		}
		return b
	}
	// Unknown node kind: encode nothing the decoder would accept; the store
	// site drops the value and the message stays uncached.
	return append(b, 0xff)
}

func decodeBSNode(b []byte, depth int) (imaplib.BodyStructure, []byte, error) {
	if depth > 64 || len(b) < 1 {
		return nil, nil, errors.New("bs depth/short")
	}
	kind := b[0]
	b = b[1:]
	var err error
	switch kind {
	case bsKindSingle:
		p := &imaplib.BodyStructureSinglePart{}
		if p.Type, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if p.Subtype, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if p.Params, b, err = getParams(b); err != nil {
			return nil, nil, err
		}
		if p.ID, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if p.Description, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if p.Encoding, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if p.Size, b, err = getU32(b); err != nil {
			return nil, nil, err
		}
		if len(b) < 1 {
			return nil, nil, errors.New("short rfc822 flag")
		}
		hasMsg := b[0]
		b = b[1:]
		if hasMsg == 1 {
			msg := &imaplib.BodyStructureMessageRFC822{}
			var n uint32
			if n, b, err = getU32(b); err != nil || uint32(len(b)) < n {
				return nil, nil, errors.New("short embedded envelope")
			}
			if n > 0 {
				env, ok := decodeEnvelope(b[:n])
				if !ok {
					return nil, nil, errors.New("embedded envelope")
				}
				msg.Envelope = env
			}
			b = b[n:]
			if len(b) < 1 {
				return nil, nil, errors.New("short embedded bs flag")
			}
			hasBS := b[0]
			b = b[1:]
			if hasBS == 1 {
				if msg.BodyStructure, b, err = decodeBSNode(b, depth+1); err != nil {
					return nil, nil, err
				}
			}
			if msg.NumLines, b, err = getI64(b); err != nil {
				return nil, nil, err
			}
			p.MessageRFC822 = msg
		}
		if len(b) < 1 {
			return nil, nil, errors.New("short text flag")
		}
		hasText := b[0]
		b = b[1:]
		if hasText == 1 {
			t := &imaplib.BodyStructureText{}
			if t.NumLines, b, err = getI64(b); err != nil {
				return nil, nil, err
			}
			p.Text = t
		}
		if len(b) < 1 {
			return nil, nil, errors.New("short ext flag")
		}
		hasExt := b[0]
		b = b[1:]
		if hasExt == 1 {
			e := &imaplib.BodyStructureSinglePartExt{}
			if e.Disposition, b, err = getDisposition(b); err != nil {
				return nil, nil, err
			}
			if e.Language, b, err = getStrs(b); err != nil {
				return nil, nil, err
			}
			if e.Location, b, err = getStr(b); err != nil {
				return nil, nil, err
			}
			p.Extended = e
		}
		return p, b, nil
	case bsKindMulti:
		p := &imaplib.BodyStructureMultiPart{}
		if p.Subtype, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		var n uint32
		if n, b, err = getU32(b); err != nil || n > 1<<12 {
			return nil, nil, errors.New("children count")
		}
		for i := uint32(0); i < n; i++ {
			var c imaplib.BodyStructure
			if c, b, err = decodeBSNode(b, depth+1); err != nil {
				return nil, nil, err
			}
			p.Children = append(p.Children, c)
		}
		if len(b) < 1 {
			return nil, nil, errors.New("short mext flag")
		}
		hasExt := b[0]
		b = b[1:]
		if hasExt == 1 {
			e := &imaplib.BodyStructureMultiPartExt{}
			if e.Params, b, err = getParams(b); err != nil {
				return nil, nil, err
			}
			if e.Disposition, b, err = getDisposition(b); err != nil {
				return nil, nil, err
			}
			if e.Language, b, err = getStrs(b); err != nil {
				return nil, nil, err
			}
			if e.Location, b, err = getStr(b); err != nil {
				return nil, nil, err
			}
			p.Extended = e
		}
		return p, b, nil
	}
	return nil, nil, errors.New("unknown bs kind")
}

func encodeBodyStructure(bs imaplib.BodyStructure) []byte {
	return encodeBSNode([]byte{bsCodecVersion}, bs)
}

func decodeBodyStructure(b []byte) (imaplib.BodyStructure, bool) {
	if len(b) < 2 || b[0] != bsCodecVersion {
		return nil, false
	}
	bs, _, err := decodeBSNode(b[1:], 0)
	if err != nil {
		return nil, false
	}
	return bs, true
}
