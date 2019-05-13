package dmr

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

const (
	// n_DFragMax, see DMR AI spec. page 163.
	MaxPacketFragmentSize = 1500
)

// CRC Masks for data block's CRC-9 calculation, see DMR AI spec. page 148 (Table B.21).
var crc9Masks = map[uint8]uint16{
	Rate12Data: 0x00f0,
	Rate34Data: 0x01ff,
	//Rate1Data: 0x010f,
}

func calculateCRC9(serial uint8, data []byte, dataType uint8) (crc uint16) {
	for _, block := range data {
		crc9(&crc, block, 8)
	}
	crc9(&crc, serial, 7)
	crc9end(&crc, 8)

	// Inverting according to the inversion polynomial.
	crc = ^crc
	crc &= 0x01ff

	// Applying Data Type CRC Mask
	crc ^= crc9Masks[dataType]

	return crc
}

var dataBlockLengths = map[uint8]int{
	Rate12Data: 12,
	Rate34Data: 18,
	Data:       22,
}

type DataBlock struct {
	Serial uint8
	CRC    uint16
	OK     bool
	Data   []byte
	Length uint8
}

func ParseDataBlock(data []byte, dataType uint8, confirmed bool) (*DataBlock, error) {
	var (
		crc uint16
		db  = &DataBlock{
			Length: uint8(userDataLength(dataType, confirmed)),
		}
	)

	if confirmed {
		db.Serial = data[0] >> 1
		db.CRC = uint16(data[0]&B00000001)<<8 | uint16(data[1])
		db.Data = make([]byte, db.Length)
		copy(db.Data, data[2:2+db.Length])

		crc = calculateCRC9(db.Serial, db.Data, dataType)

		// FIXME(pd0mz): this is not working
		if crc != db.CRC {
			return nil, fmt.Errorf("dmr: block CRC error (%#04x != %#04x)", crc, db.CRC)
		}
	} else {
		db.Data = make([]byte, db.Length)
		copy(db.Data, data[:db.Length])
	}

	db.OK = true
	return db, nil
}

func (db *DataBlock) Bytes(dataType uint8, confirmed bool) []byte {
	data := make([]byte, dataBlockLengths[dataType])

	if confirmed {
		db.CRC = calculateCRC9(db.Serial, db.Data, dataType)

		data[0] = (db.Serial << 1) | (uint8(db.CRC>>8) & 0x01)
		data[1] = uint8(db.CRC)
		copy(data[2:], db.Data)
	} else {
		copy(data, db.Data)
	}

	return data
}

func userDataLength(dataType uint8, confirmed bool) int {
	size, ok := dataBlockLengths[dataType]
	if ok && confirmed {
		return size - 2
	}
	return size
}

type DataFragment struct {
	Data   []byte
	Stored int
	Needed int
	CRC    uint32
}

func (df *DataFragment) DataBlocks(dataType uint8, confirm bool) ([]*DataBlock, error) {
	df.Stored = len(df.Data)
	if df.Stored > MaxPacketFragmentSize {
		df.Stored = MaxPacketFragmentSize
	}

	// See DMR AI spec. page. 73. for block sizes.
	blockCap := userDataLength(dataType, confirm)
	df.Needed = (df.Stored + blockCap - 1) / blockCap

	// Leave enough room for the 4 bytes CRC32
	if (df.Needed*blockCap)-df.Stored < 4 {
		df.Needed++
	}

	// Calculate fragment CRC32
	for i := 0; i < (df.Needed*blockCap)-4; i += 2 {
		if i+1 < df.Stored {
			crc32(&df.CRC, df.Data[i+1])
		} else {
			crc32(&df.CRC, 0)
		}
		if i < df.Stored {
			crc32(&df.CRC, df.Data[i])
		} else {
			crc32(&df.CRC, 0)
		}
	}
	crc32end(&df.CRC)

	var (
		blocks = make([]*DataBlock, df.Needed)
		stored int
	)
	for i := range blocks {
		block := &DataBlock{
			Serial: uint8(i % 128),
			Length: uint8(blockCap),
		}
		block.Data = make([]byte, block.Length)

		store := int(block.Length)
		if df.Stored-stored < store {
			store = df.Stored - stored
		}
		copy(block.Data, df.Data[stored:stored+store])
		stored += store

		if i == (df.Needed - 1) {
			block.Data[block.Length-1] = uint8(df.CRC >> 24)
			block.Data[block.Length-2] = uint8(df.CRC >> 16)
			block.Data[block.Length-3] = uint8(df.CRC >> 8)
			block.Data[block.Length-4] = uint8(df.CRC)
		}

		// Calculate block CRC9
		block.CRC = calculateCRC9(block.Serial, block.Data, dataType)

		blocks[i] = block
	}

	return blocks, nil
}

func CombineDataBlocks(blocks []*DataBlock) (*DataFragment, error) {
	if blocks == nil || len(blocks) == 0 {
		return nil, errors.New("dmr: no data blocks to combine")
	}

	f := &DataFragment{
		Data: make([]byte, MaxPacketFragmentSize),
	}
	for i, block := range blocks {
		if block.Length == 0 {
			continue
		}
		if i < (len(blocks) - 1) {
			if f.Stored+int(block.Length) < len(f.Data) {
				copy(f.Data[f.Stored:], block.Data[:block.Length])
				f.Stored += int(block.Length)
			}
		} else {
			if f.Stored+int(block.Length)-4 < len(f.Data) {
				copy(f.Data[f.Stored:], block.Data[:block.Length])
				f.Stored += int(block.Length)
			}
			f.CRC = 0
			f.CRC |= uint32(block.Data[block.Length-4])
			f.CRC |= uint32(block.Data[block.Length-3]) << 8
			f.CRC |= uint32(block.Data[block.Length-2]) << 16
			f.CRC |= uint32(block.Data[block.Length-1]) << 24
		}
	}

	var crc uint32
	for i := 0; i < f.Stored-4; i += 2 {
		crc32(&crc, f.Data[i+1])
		crc32(&crc, f.Data[i])
	}
	crc32end(&crc)

	if crc != f.CRC {
		return nil, fmt.Errorf("dmr: fragment CRC error (%#08x != %#08x)", crc, f.CRC)
	}
	return f, nil
}

var encodingMap map[uint8]encoding.Encoding

func BuildMessageData(msg string, ddFormat uint8, nullTerminated bool) ([]byte, error) {
	if e, ok := encodingMap[ddFormat]; ok {
		enc := e.NewEncoder()
		data, err := enc.Bytes([]byte(msg))
		if err != nil {
			return nil, err
		}
		if nullTerminated {
			data = append(data, []byte{0x00, 0x00}...)
		}
		return data, nil
	}
	return nil, fmt.Errorf("dmr: encoding %s text is not supported", DDFormatName[ddFormat])
}

func ParseMessageData(data []byte, ddFormat uint8, nullTerminated bool) (string, error) {
	if e, ok := encodingMap[ddFormat]; ok {
		dec := e.NewDecoder()
		str, err := dec.Bytes(data)
		if err != nil {
			return "", err
		}
		if nullTerminated {
			if idx := bytes.IndexByte(str, 0x00); idx >= 0 {
				str = str[:idx]
			}
		}
		return string(str), nil
	}
	return "", fmt.Errorf("dmr: decoding %s text is not supported", DDFormatName[ddFormat])
}

func init() {
	encodingMap = map[uint8]encoding.Encoding{
		DDFormatBinary:         binaryEncoding{},
		DDFormat8BitISO8859_2:  charmap.ISO8859_2,
		DDFormat8BitISO8859_3:  charmap.ISO8859_3,
		DDFormat8BitISO8859_4:  charmap.ISO8859_4,
		DDFormat8BitISO8859_5:  charmap.ISO8859_5,
		DDFormat8BitISO8859_6:  charmap.ISO8859_6,
		DDFormat8BitISO8859_7:  charmap.ISO8859_7,
		DDFormat8BitISO8859_8:  charmap.ISO8859_8,
		DDFormat8BitISO8859_10: charmap.ISO8859_10,
		DDFormat8BitISO8859_13: charmap.ISO8859_13,
		DDFormat8BitISO8859_14: charmap.ISO8859_14,
		DDFormat8BitISO8859_15: charmap.ISO8859_15,
		DDFormat8BitISO8859_16: charmap.ISO8859_16,
		DDFormatUTF8:           unicode.UTF8,
		DDFormatUTF16:          unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
		DDFormatUTF16BE:        unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
		DDFormatUTF16LE:        unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	}
}
