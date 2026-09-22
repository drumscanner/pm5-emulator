package csafe

import (
	"errors"
)

// Decoder can decode the raw data - considering it as a csafe-encoded packet.
type Decoder struct {
}

// Decode decodes the raw csafe-encoded data, treating the whole frame as a
// single command (with optional trailing data). This preserves the original
// decoding behavior and is kept for backward compatibility; frames that chain
// multiple commands together should use DecodeAll instead.
func (d *Decoder) Decode(raw []byte) (*Packet, error) {
	pck, err := d.decodeFrame(raw)
	if err != nil {
		return nil, err
	}

	// Extract Command
	cmd := pck[0]

	if len(pck) == 1 {
		// Command only
		p := &Packet{
			Data:    nil,
			Cmds:    []byte{cmd},
			JustCmd: true,
		}

		return p, nil
	}

	// Extract the data length
	dataLen := pck[1]

	p := &Packet{
		Data:    make([]byte, dataLen),
		Cmds:    []byte{cmd},
		JustCmd: false,
	}

	// Extract data
	for i := 0; i < int(dataLen); i++ {
		p.Data[i] = pck[2+i]
	}

	return p, nil
}

// DecodeAll decodes a raw csafe-encoded frame that may chain several
// commands together, as real CSAFE clients (e.g. ErgData) commonly do. Each
// command becomes its own Packet, in the order it appeared in the frame.
// Short commands (cmd&SHORT_CMD_TYPE_MSK != 0) carry no data; long commands
// are followed by a byte count and that many data bytes.
func (d *Decoder) DecodeAll(raw []byte) ([]*Packet, error) {
	dta, err := d.decodeFrame(raw)
	if err != nil {
		return nil, err
	}

	var packets []*Packet
	for i := 0; i < len(dta); {
		cmd := dta[i]

		if cmd&SHORT_CMD_TYPE_MSK != 0 {
			packets = append(packets, &Packet{Data: nil, Cmds: []byte{cmd}, JustCmd: true})
			i++
			continue
		}

		if i+1 >= len(dta) {
			return nil, errors.New("truncated long command: missing byte count")
		}
		dataLen := int(dta[i+1])
		if i+2+dataLen > len(dta) {
			return nil, errors.New("truncated long command: data length exceeds frame")
		}

		data := make([]byte, dataLen)
		copy(data, dta[i+2:i+2+dataLen])
		packets = append(packets, &Packet{Data: data, Cmds: []byte{cmd}, JustCmd: false})
		i += 2 + dataLen
	}

	return packets, nil
}

// decodeFrame strips framing, reverses byte-stuffing and validates the
// checksum, returning the command+data bytes (without the checksum byte).
func (d *Decoder) decodeFrame(raw []byte) ([]byte, error) {
	if len(raw) < 4 {
		return nil, errors.New("raw data length less than minimum length")
	}

	// Remove frame start and end bytes
	body, err := d.stripHeadTail(raw)
	if err != nil {
		return nil, err
	}

	// Perform reverse byte-stuffing
	pck, err := d.unstuff(body)
	if err != nil {
		return nil, err
	}

	// Check the checksum
	dta := pck[0 : len(pck)-1]
	checksum := calculateChecksum(dta)
	if checksum != pck[len(pck)-1] {
		return nil, errors.New("checksum mismatched")
	}

	return dta, nil
}

// stripHeadTail removes the framing head and tail bytes.
func (d *Decoder) stripHeadTail(raw []byte) ([]byte, error) {
	if raw[0] != FRAME_START_BYTE || raw[len(raw)-1] != FRAME_END_BYTE {
		return raw, errors.New("not a packet")
	}

	return raw[1 : len(raw)-1], nil
}

// unstuff performs the reverse operation of csafe byte-stuffing.
func (d *Decoder) unstuff(raw []byte) ([]byte, error) {
	var buffer []byte

	for i := 0; i < len(raw); i++ {
		curByte := raw[i]
		if curByte == FRAME_STUFF_BYTE {
			if (i == len(raw)-1) || (0b11111100&raw[i+1]) != 0 {
				return raw, errors.New("unspecified Format")
			}
			buffer = append(buffer, 0xF0|raw[i+1])
			i++
		} else {
			buffer = append(buffer, curByte)
		}
	}

	return buffer, nil
}
