package binary

import (
	"fmt"
	"io"
	"strings"

	"go.mau.fi/whatsmeow/binary/token"
	"go.mau.fi/whatsmeow/types"
)

// Pre-boxed token values so returning a token as `any` doesn't heap-allocate a string header per read.
var (
	singleByteTokenIfaces = func() []any {
		out := make([]any, len(token.SingleByteTokens))
		for i, t := range token.SingleByteTokens {
			out[i] = t
		}
		return out
	}()
	doubleByteTokenIfaces = func() [][]any {
		out := make([][]any, len(token.DoubleByteTokens))
		for i, dict := range token.DoubleByteTokens {
			out[i] = make([]any, len(dict))
			for j, t := range dict {
				out[i][j] = t
			}
		}
		return out
	}()
)

type binaryDecoder struct {
	data  []byte
	index int
}

func newDecoder(data []byte) *binaryDecoder {
	return &binaryDecoder{data, 0}
}

func (r *binaryDecoder) checkEOS(length int) error {
	if r.index+length > len(r.data) {
		return io.EOF
	}

	return nil
}

func (r *binaryDecoder) readByte() (byte, error) {
	if err := r.checkEOS(1); err != nil {
		return 0, err
	}

	b := r.data[r.index]
	r.index++

	return b, nil
}

func (r *binaryDecoder) readIntN(n int, littleEndian bool) (int, error) {
	if err := r.checkEOS(n); err != nil {
		return 0, err
	}

	var ret int

	for i := 0; i < n; i++ {
		var curShift int
		if littleEndian {
			curShift = i
		} else {
			curShift = n - i - 1
		}
		ret |= int(r.data[r.index+i]) << uint(curShift*8)
	}

	r.index += n
	return ret, nil
}

func (r *binaryDecoder) readInt8(littleEndian bool) (int, error) {
	return r.readIntN(1, littleEndian)
}

func (r *binaryDecoder) readInt16(littleEndian bool) (int, error) {
	return r.readIntN(2, littleEndian)
}

func (r *binaryDecoder) readInt20() (int, error) {
	if err := r.checkEOS(3); err != nil {
		return 0, err
	}

	ret := ((int(r.data[r.index]) & 15) << 16) + (int(r.data[r.index+1]) << 8) + int(r.data[r.index+2])
	r.index += 3
	return ret, nil
}

func (r *binaryDecoder) readInt32(littleEndian bool) (int, error) {
	return r.readIntN(4, littleEndian)
}

func (r *binaryDecoder) readPacked8(tag int) (string, error) {
	startByte, err := r.readByte()
	if err != nil {
		return "", err
	}

	var build strings.Builder
	build.Grow(2 * int(startByte&127))

	for i := 0; i < int(startByte&127); i++ {
		currByte, err := r.readByte()
		if err != nil {
			return "", err
		}

		lower, err := unpackByte(tag, currByte&0xF0>>4)
		if err != nil {
			return "", err
		}

		upper, err := unpackByte(tag, currByte&0x0F)
		if err != nil {
			return "", err
		}

		build.WriteByte(lower)
		build.WriteByte(upper)
	}

	ret := build.String()
	if startByte>>7 != 0 && len(ret) > 0 {
		ret = ret[:len(ret)-1]
	}
	return ret, nil
}

func unpackByte(tag int, value byte) (byte, error) {
	switch tag {
	case token.Nibble8:
		return unpackNibble(value)
	case token.Hex8:
		return unpackHex(value)
	default:
		return 0, fmt.Errorf("unpackByte with unknown tag %d", tag)
	}
}

func unpackNibble(value byte) (byte, error) {
	switch {
	case value < 10:
		return '0' + value, nil
	case value == 10:
		return '-', nil
	case value == 11:
		return '.', nil
	case value == 15:
		return 0, nil
	default:
		return 0, fmt.Errorf("unpackNibble with value %d", value)
	}
}

func unpackHex(value byte) (byte, error) {
	switch {
	case value < 10:
		return '0' + value, nil
	case value < 16:
		return 'A' + value - 10, nil
	default:
		return 0, fmt.Errorf("unpackHex with value %d", value)
	}
}

func (r *binaryDecoder) readListSize(tag int) (int, error) {
	switch tag {
	case token.ListEmpty:
		return 0, nil
	case token.List8:
		return r.readInt8(false)
	case token.List16:
		return r.readInt16(false)
	default:
		return 0, fmt.Errorf("readListSize with unknown tag %d at position %d", tag, r.index)
	}
}

func (r *binaryDecoder) read(string bool) (any, error) {
	tagByte, err := r.readByte()
	if err != nil {
		return nil, err
	}
	tag := int(tagByte)
	switch tag {
	case token.ListEmpty:
		return nil, nil
	case token.List8, token.List16:
		return r.readList(tag)
	case token.Binary8:
		size, err := r.readInt8(false)
		if err != nil {
			return nil, err
		}

		return r.readBytesOrString(size, string)
	case token.Binary20:
		size, err := r.readInt20()
		if err != nil {
			return nil, err
		}

		return r.readBytesOrString(size, string)
	case token.Binary32:
		size, err := r.readInt32(false)
		if err != nil {
			return nil, err
		}

		return r.readBytesOrString(size, string)
	case token.Dictionary0, token.Dictionary1, token.Dictionary2, token.Dictionary3:
		i, err := r.readInt8(false)
		if err != nil {
			return "", err
		}

		if d := tag - token.Dictionary0; i >= 0 && i < len(doubleByteTokenIfaces[d]) {
			return doubleByteTokenIfaces[d][i], nil
		}
		return token.GetDoubleToken(tag-token.Dictionary0, i)
	case token.FBJID:
		return r.readFBJID()
	case token.InteropJID:
		return r.readInteropJID()
	case token.JIDPair:
		return r.readJIDPair()
	case token.ADJID:
		return r.readADJID()
	case token.Nibble8, token.Hex8:
		return r.readPacked8(tag)
	default:
		if tag >= 1 && tag < len(token.SingleByteTokens) {
			return singleByteTokenIfaces[tag], nil
		}
		return "", fmt.Errorf("%w %d at position %d", ErrInvalidToken, tag, r.index)
	}
}

// readString reads a value that must be a string (or empty). A malformed
// frame can put a JID or list there; that's an error, not a panic.
func (r *binaryDecoder) readString() (string, bool, error) {
	val, err := r.read(true)
	if err != nil {
		return "", false, err
	}
	switch v := val.(type) {
	case nil:
		return "", false, nil
	case string:
		return v, true, nil
	default:
		return "", false, fmt.Errorf("%w: expected string at position %d, got %T", ErrInvalidType, r.index, val)
	}
}

func (r *binaryDecoder) readJIDPair() (any, error) {
	user, _, err := r.readString()
	if err != nil {
		return nil, err
	}
	server, hasServer, err := r.readString()
	if err != nil {
		return nil, err
	} else if !hasServer {
		return nil, ErrInvalidJIDType
	}
	return types.NewJID(user, server), nil
}

func (r *binaryDecoder) readInteropJID() (any, error) {
	user, _, err := r.readString()
	if err != nil {
		return nil, err
	}
	device, err := r.readInt16(false)
	if err != nil {
		return nil, err
	}
	integrator, err := r.readInt16(false)
	if err != nil {
		return nil, err
	}
	server, err := r.read(true)
	if err != nil {
		return nil, err
	} else if server != types.InteropServer {
		return nil, fmt.Errorf("%w: expected %q, got %q", ErrInvalidJIDType, types.InteropServer, server)
	}
	return types.JID{
		User:       user,
		Device:     uint16(device),
		Integrator: uint16(integrator),
		Server:     types.InteropServer,
	}, nil
}

func (r *binaryDecoder) readFBJID() (any, error) {
	user, _, err := r.readString()
	if err != nil {
		return nil, err
	}
	device, err := r.readInt16(false)
	if err != nil {
		return nil, err
	}
	server, err := r.read(true)
	if err != nil {
		return nil, err
	} else if server != types.MessengerServer {
		return nil, fmt.Errorf("%w: expected %q, got %q", ErrInvalidJIDType, types.MessengerServer, server)
	}
	return types.JID{
		User:   user,
		Device: uint16(device),
		Server: types.MessengerServer,
	}, nil
}

func (r *binaryDecoder) readADJID() (any, error) {
	agent, err := r.readByte()
	if err != nil {
		return nil, err
	}
	device, err := r.readByte()
	if err != nil {
		return nil, err
	}
	user, _, err := r.readString()
	if err != nil {
		return nil, err
	}
	return types.NewADJID(user, agent, device), nil
}

func (r *binaryDecoder) readAttributes(n int) (Attrs, error) {
	if n == 0 {
		return nil, nil
	}

	ret := make(Attrs, n)
	for i := 0; i < n; i++ {
		keyIfc, err := r.read(true)
		if err != nil {
			return nil, err
		}

		key, ok := keyIfc.(string)
		if !ok {
			return nil, fmt.Errorf("%[1]w at position %[3]d (%[2]T): %+[2]v", ErrNonStringKey, key, r.index)
		}

		ret[key], err = r.read(true)
		if err != nil {
			return nil, err
		}
	}

	return ret, nil
}

func (r *binaryDecoder) readList(tag int) ([]Node, error) {
	size, err := r.readListSize(tag)
	if err != nil {
		return nil, err
	}

	ret := make([]Node, size)
	for i := 0; i < size; i++ {
		if err := r.readNodeInto(&ret[i]); err != nil {
			return nil, err
		}
	}

	return ret, nil
}

func (r *binaryDecoder) readNode() (*Node, error) {
	ret := &Node{}
	if err := r.readNodeInto(ret); err != nil {
		return nil, err
	}
	return ret, nil
}

func (r *binaryDecoder) readNodeInto(ret *Node) error {

	size, err := r.readInt8(false)
	if err != nil {
		return err
	}
	listSize, err := r.readListSize(size)
	if err != nil {
		return err
	}

	ret.Tag, _, err = r.readString()
	if err != nil {
		return err
	}
	if listSize == 0 || ret.Tag == "" {
		return ErrInvalidNode
	}

	ret.Attrs, err = r.readAttributes((listSize - 1) >> 1)
	if err != nil {
		return err
	}

	if listSize%2 == 1 {
		return nil
	}

	ret.Content, err = r.read(false)
	return err
}

func (r *binaryDecoder) readBytesOrString(length int, asString bool) (any, error) {
	data, err := r.readRaw(length)
	if err != nil {
		return nil, err
	}
	if asString {
		return string(data), nil
	}
	return data, nil
}

func (r *binaryDecoder) readRaw(length int) ([]byte, error) {
	if err := r.checkEOS(length); err != nil {
		return nil, err
	}

	ret := r.data[r.index : r.index+length]
	r.index += length

	return ret, nil
}
