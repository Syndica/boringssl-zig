// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DTLS implementation.
//
// NOTE: This is a not even a remotely production-quality DTLS
// implementation. It is the bare minimum necessary to be able to
// achieve coverage on BoringSSL's implementation. Of note is that
// this implementation assumes the underlying net.PacketConn is not
// only reliable but also ordered. BoringSSL will be expected to deal
// with simulated loss, but there is no point in forcing the test
// driver to.

package runner

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"slices"

	"golang.org/x/crypto/cryptobyte"
)

func (c *Conn) readDTLS13RecordHeader(b []byte) (headerLen int, recordLen int, recTyp recordType, seq []byte, err error) {
	// The DTLS 1.3 record header starts with the type byte containing
	// 0b001CSLEE, where C, S, L, and EE are bits with the following
	// meanings:
	//
	// C=1: Connection ID is present (C=0: CID is absent)
	// S=1: the sequence number is 16 bits (S=0: it is 8 bits)
	// L=1: 16-bit length field is present (L=0: record goes to end of packet)
	// EE: low two bits of the epoch.
	//
	// A real DTLS implementation would parse these bits and take
	// appropriate action based on them. However, this is a test
	// implementation, and the code we are testing only ever sends C=0, S=1,
	// L=1. This code expects those bits to be set and will error if
	// anything else is set. This means we expect the type byte to look like
	// 0b001011EE, or 0x2c-0x2f.
	recordHeaderLen := 5
	if len(b) < recordHeaderLen {
		return 0, 0, 0, nil, errors.New("dtls: failed to read record header")
	}
	typ := b[0]
	if typ&0xfc != 0x2c {
		return 0, 0, 0, nil, errors.New("dtls: DTLS 1.3 record header has bad type byte")
	}
	// For test purposes, require the epoch received be the same as the
	// epoch we expect to receive.
	epoch := typ & 0x03
	if epoch != c.in.seq[1]&0x03 {
		c.sendAlert(alertIllegalParameter)
		return 0, 0, 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: bad epoch"))
	}
	wireSeq := b[1:3]
	if !c.config.Bugs.NullAllCiphers {
		sample := b[recordHeaderLen:]
		mask := c.in.recordNumberEncrypter.generateMask(sample)
		xorSlice(wireSeq, mask)
	}
	decWireSeq := binary.BigEndian.Uint16(wireSeq)
	// Reconstruct the sequence number from the low 16 bits on the wire.
	// A real implementation would compute the full sequence number that is
	// closest to the highest successfully decrypted record in the
	// identified epoch. Since this test implementation errors on decryption
	// failures instead of simply discarding packets, it reconstructs a
	// sequence number that is not less than c.in.seq. (This matches the
	// behavior of the check of the sequence number in the old record
	// header format.)
	seqInt := binary.BigEndian.Uint64(c.in.seq[:])
	// c.in.seq has the epoch in the upper two bytes - clear those.
	seqInt = seqInt &^ (0xffff << 48)
	newSeq := seqInt&^0xffff | uint64(decWireSeq)
	if newSeq < seqInt {
		newSeq += 0x10000
	}

	seq = make([]byte, 8)
	binary.BigEndian.PutUint64(seq, newSeq)
	copy(c.in.seq[2:], seq[2:])

	recordLen = int(b[3])<<8 | int(b[4])
	return recordHeaderLen, recordLen, 0, seq, nil
}

// readDTLSRecordHeader reads the record header from the input. Based on the
// header it reads, it checks the header's validity and sets appropriate state
// as needed. This function returns the record header, the record type indicated
// in the header (if it contains the type), and the sequence number to use for
// record decryption.
func (c *Conn) readDTLSRecordHeader(b []byte) (headerLen int, recordLen int, typ recordType, seq []byte, err error) {
	if c.in.cipher != nil && c.in.version >= VersionTLS13 {
		return c.readDTLS13RecordHeader(b)
	}

	recordHeaderLen := 13
	// Read out one record.
	//
	// A real DTLS implementation should be tolerant of errors,
	// but this is test code. We should not be tolerant of our
	// peer sending garbage.
	if len(b) < recordHeaderLen {
		return 0, 0, 0, nil, errors.New("dtls: failed to read record header")
	}
	typ = recordType(b[0])
	vers := uint16(b[1])<<8 | uint16(b[2])
	// Alerts sent near version negotiation do not have a well-defined
	// record-layer version prior to TLS 1.3. (In TLS 1.3, the record-layer
	// version is irrelevant.)
	if typ != recordTypeAlert {
		if c.haveVers {
			wireVersion := c.wireVersion
			if c.vers >= VersionTLS13 {
				wireVersion = VersionDTLS12
			}
			if vers != wireVersion {
				c.sendAlert(alertProtocolVersion)
				return 0, 0, 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: received record with version %x when expecting version %x", vers, c.wireVersion))
			}
		} else {
			// Pre-version-negotiation alerts may be sent with any version.
			if expect := c.config.Bugs.ExpectInitialRecordVersion; expect != 0 && vers != expect {
				c.sendAlert(alertProtocolVersion)
				return 0, 0, 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: received record with version %x when expecting version %x", vers, expect))
			}
		}
	}
	epoch := b[3:5]
	seq = b[5:11]
	// For test purposes, require the sequence number be monotonically
	// increasing, so c.in includes the minimum next sequence number. Gaps
	// may occur if packets failed to be sent out. A real implementation
	// would maintain a replay window and such.
	if !bytes.Equal(epoch, c.in.seq[:2]) {
		c.sendAlert(alertIllegalParameter)
		return 0, 0, 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: bad epoch"))
	}
	if bytes.Compare(seq, c.in.seq[2:]) < 0 {
		c.sendAlert(alertIllegalParameter)
		return 0, 0, 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: bad sequence number"))
	}
	copy(c.in.seq[2:], seq)
	recordLen = int(b[11])<<8 | int(b[12])
	return recordHeaderLen, recordLen, typ, b[3:11], nil
}

func (c *Conn) writeACKs(seqnums []uint64) {
	recordNumbers := new(cryptobyte.Builder)
	epoch := binary.BigEndian.Uint16(c.in.seq[:2])
	recordNumbers.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		for _, seq := range seqnums {
			b.AddUint64(uint64(epoch))
			b.AddUint64(seq)
		}
	})
	c.writeRecord(recordTypeACK, recordNumbers.BytesOrPanic())
}

func (c *Conn) dtlsDoReadRecord(want recordType) (recordType, []byte, error) {
	// Read a new packet only if the current one is empty.
	var newPacket bool
	if c.rawInput.Len() == 0 {
		// Pick some absurdly large buffer size.
		c.rawInput.Grow(maxCiphertext + dtlsMaxRecordHeaderLen)
		buf := c.rawInput.AvailableBuffer()
		n, err := c.conn.Read(buf[:cap(buf)])
		if err != nil {
			return 0, nil, err
		}
		if c.config.Bugs.MaxPacketLength != 0 && n > c.config.Bugs.MaxPacketLength {
			return 0, nil, fmt.Errorf("dtls: exceeded maximum packet length")
		}
		c.rawInput.Write(buf[:n])
		newPacket = true
	}

	// Consume the next record from the buffer.
	recordHeaderLen, n, typ, seq, err := c.readDTLSRecordHeader(c.rawInput.Bytes())
	if err != nil {
		return 0, nil, err
	}
	if n > maxCiphertext || c.rawInput.Len() < recordHeaderLen+n {
		c.sendAlert(alertRecordOverflow)
		return 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: oversized record received with length %d", n))
	}
	b := c.rawInput.Next(recordHeaderLen + n)

	// Process message.
	ok, encTyp, data, alertValue := c.in.decrypt(seq, recordHeaderLen, b)
	if !ok {
		// A real DTLS implementation would silently ignore bad records,
		// but we want to notice errors from the implementation under
		// test.
		return 0, nil, c.in.setErrorLocked(c.sendAlert(alertValue))
	}
	if c.config.Bugs.ACKEveryRecord {
		c.writeACKs([]uint64{binary.BigEndian.Uint64(seq)})
	}

	if typ == 0 {
		// readDTLSRecordHeader sets typ=0 when decoding the DTLS 1.3
		// record header. When the new record header format is used, the
		// type is returned by decrypt() in encTyp.
		typ = encTyp
	}

	// Require that ChangeCipherSpec always share a packet with either the
	// previous or next handshake message.
	if newPacket && typ == recordTypeChangeCipherSpec && c.rawInput.Len() == 0 {
		return 0, nil, c.in.setErrorLocked(fmt.Errorf("dtls: ChangeCipherSpec not packed together with Finished"))
	}

	return typ, data, nil
}

func (c *Conn) makeFragment(header, data []byte, fragOffset, fragLen int) []byte {
	fragment := make([]byte, 0, 12+fragLen)
	fragment = append(fragment, header...)
	fragment = append(fragment, byte(c.sendHandshakeSeq>>8), byte(c.sendHandshakeSeq))
	fragment = append(fragment, byte(fragOffset>>16), byte(fragOffset>>8), byte(fragOffset))
	fragment = append(fragment, byte(fragLen>>16), byte(fragLen>>8), byte(fragLen))
	fragment = append(fragment, data[fragOffset:fragOffset+fragLen]...)
	return fragment
}

func (c *Conn) dtlsWriteRecord(typ recordType, data []byte) (n int, err error) {
	// Don't send ChangeCipherSpec in DTLS 1.3.
	// TODO(crbug.com/42290594): Add an option to send them anyway and test
	// what our implementation does with unexpected ones.
	if typ == recordTypeChangeCipherSpec && c.vers >= VersionTLS13 {
		return
	}

	// Only handshake messages are fragmented.
	if typ != recordTypeHandshake {
		reorder := typ == recordTypeChangeCipherSpec && c.config.Bugs.ReorderChangeCipherSpec

		// Flush pending handshake messages before encrypting a new record.
		if !reorder {
			err = c.dtlsPackHandshake()
			if err != nil {
				return
			}
		}

		if typ == recordTypeApplicationData && len(data) > 1 && c.config.Bugs.SplitAndPackAppData {
			_, err = c.dtlsPackRecord(typ, data[:len(data)/2], false)
			if err != nil {
				return
			}
			_, err = c.dtlsPackRecord(typ, data[len(data)/2:], true)
			if err != nil {
				return
			}
			n = len(data)
		} else {
			n, err = c.dtlsPackRecord(typ, data, false)
			if err != nil {
				return
			}
		}

		if reorder {
			err = c.dtlsPackHandshake()
			if err != nil {
				return
			}
		}

		if typ == recordTypeChangeCipherSpec && c.vers < VersionTLS13 {
			err = c.out.changeCipherSpec(c.config)
			if err != nil {
				return n, c.sendAlertLocked(alertLevelError, err.(alert))
			}
		} else {
			// ChangeCipherSpec is part of the handshake and not
			// flushed until dtlsFlushPacket.
			err = c.dtlsFlushPacket()
			if err != nil {
				return
			}
		}
		return
	}

	if c.out.cipher == nil && c.config.Bugs.StrayChangeCipherSpec {
		_, err = c.dtlsPackRecord(recordTypeChangeCipherSpec, []byte{1}, false)
		if err != nil {
			return
		}
	}

	maxLen := c.config.Bugs.MaxHandshakeRecordLength
	if maxLen <= 0 {
		maxLen = 1024
	}

	// Handshake messages have to be modified to include fragment
	// offset and length and with the header replicated. Save the
	// TLS header here.
	//
	// TODO(davidben): This assumes that data contains exactly one
	// handshake message. This is incompatible with
	// FragmentAcrossChangeCipherSpec. (Which is unfortunate
	// because OpenSSL's DTLS implementation will probably accept
	// such fragmentation and could do with a fix + tests.)
	header := data[:4]
	data = data[4:]

	isFinished := header[0] == typeFinished

	if c.config.Bugs.SendEmptyFragments {
		c.pendingFragments = append(c.pendingFragments, c.makeFragment(header, data, 0, 0))
		c.pendingFragments = append(c.pendingFragments, c.makeFragment(header, data, len(data), 0))
	}

	firstRun := true
	fragOffset := 0
	for firstRun || fragOffset < len(data) {
		firstRun = false
		fragLen := len(data) - fragOffset
		if fragLen > maxLen {
			fragLen = maxLen
		}

		fragment := c.makeFragment(header, data, fragOffset, fragLen)
		if c.config.Bugs.FragmentMessageTypeMismatch && fragOffset > 0 {
			fragment[0]++
		}
		if c.config.Bugs.FragmentMessageLengthMismatch && fragOffset > 0 {
			fragment[3]++
		}

		// Buffer the fragment for later. They will be sent (and
		// reordered) on flush.
		c.pendingFragments = append(c.pendingFragments, fragment)
		if c.config.Bugs.ReorderHandshakeFragments {
			// Don't duplicate Finished to avoid the peer
			// interpreting it as a retransmit request.
			if !isFinished {
				c.pendingFragments = append(c.pendingFragments, fragment)
			}

			if fragLen > (maxLen+1)/2 {
				// Overlap each fragment by half.
				fragLen = (maxLen + 1) / 2
			}
		}
		fragOffset += fragLen
		n += fragLen
	}
	shouldSendTwice := c.config.Bugs.MixCompleteMessageWithFragments
	if isFinished {
		shouldSendTwice = c.config.Bugs.RetransmitFinished
	}
	if shouldSendTwice {
		fragment := c.makeFragment(header, data, 0, len(data))
		c.pendingFragments = append(c.pendingFragments, fragment)
	}

	// Increment the handshake sequence number for the next
	// handshake message.
	c.sendHandshakeSeq++
	return
}

// dtlsPackHandshake packs the pending handshake flight into the pending
// record. Callers should follow up with dtlsFlushPacket to write the packets.
func (c *Conn) dtlsPackHandshake() error {
	// This is a test-only DTLS implementation, so there is no need to
	// retain |c.pendingFragments| for a future retransmit.
	var fragments [][]byte
	fragments, c.pendingFragments = c.pendingFragments, fragments

	if c.config.Bugs.ReorderHandshakeFragments {
		perm := rand.New(rand.NewSource(0)).Perm(len(fragments))
		tmp := make([][]byte, len(fragments))
		for i := range tmp {
			tmp[i] = fragments[perm[i]]
		}
		fragments = tmp
	} else if c.config.Bugs.ReverseHandshakeFragments {
		tmp := make([][]byte, len(fragments))
		for i := range tmp {
			tmp[i] = fragments[len(fragments)-i-1]
		}
		fragments = tmp
	}

	maxRecordLen := c.config.Bugs.PackHandshakeFragments

	// Pack handshake fragments into records.
	var records [][]byte
	for _, fragment := range fragments {
		if n := c.config.Bugs.SplitFragments; n > 0 {
			if len(fragment) > n {
				records = append(records, fragment[:n])
				records = append(records, fragment[n:])
			} else {
				records = append(records, fragment)
			}
		} else if i := len(records) - 1; len(records) > 0 && len(records[i])+len(fragment) <= maxRecordLen {
			records[i] = append(records[i], fragment...)
		} else {
			// The fragment will be appended to, so copy it.
			records = append(records, slices.Clone(fragment))
		}
	}

	// Send the records.
	for _, record := range records {
		_, err := c.dtlsPackRecord(recordTypeHandshake, record, false)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Conn) dtlsFlushHandshake() error {
	if err := c.dtlsPackHandshake(); err != nil {
		return err
	}
	if err := c.dtlsFlushPacket(); err != nil {
		return err
	}

	return nil
}

// appendDTLS13RecordHeader appends to b the record header for a record of length
// recordLen.
func (c *Conn) appendDTLS13RecordHeader(b, seq []byte, recordLen int) []byte {
	// Set the top 3 bits on the type byte to indicate the DTLS 1.3 record
	// header format.
	typ := byte(0x20)
	// Set the Connection ID bit
	if c.config.Bugs.DTLS13RecordHeaderSetCIDBit && c.handshakeComplete {
		typ |= 0x10
	}
	// Set the sequence number length bit
	if !c.config.DTLSUseShortSeqNums {
		typ |= 0x08
	}
	// Set the length presence bit
	if !c.config.DTLSRecordHeaderOmitLength {
		typ |= 0x04
	}
	// Set the epoch bits
	typ |= seq[1] & 0x3
	b = append(b, typ)
	if c.config.DTLSUseShortSeqNums {
		b = append(b, seq[7])
	} else {
		b = append(b, seq[6], seq[7])
	}
	if !c.config.DTLSRecordHeaderOmitLength {
		b = append(b, byte(recordLen>>8), byte(recordLen))
	}
	return b
}

// dtlsPackRecord packs a single record to the pending packet, flushing it
// if necessary. The caller should call dtlsFlushPacket to flush the current
// pending packet afterwards.
func (c *Conn) dtlsPackRecord(typ recordType, data []byte, mustPack bool) (n int, err error) {
	maxLen := c.config.Bugs.MaxHandshakeRecordLength
	if maxLen <= 0 {
		maxLen = 1024
	}

	vers := c.wireVersion
	if vers == 0 {
		// Some TLS servers fail if the record version is greater than
		// TLS 1.0 for the initial ClientHello.
		if c.isDTLS {
			vers = VersionDTLS10
		} else {
			vers = VersionTLS10
		}
	}
	if c.vers >= VersionTLS13 || c.out.version >= VersionTLS13 {
		vers = VersionDTLS12
	}

	useDTLS13RecordHeader := c.out.version >= VersionTLS13 && c.out.cipher != nil && !c.useDTLSPlaintextHeader()
	headerHasLength := true
	record := make([]byte, 0, dtlsMaxRecordHeaderLen+len(data)+c.out.maxEncryptOverhead(len(data)))
	seq := c.out.sequenceNumberForOutput()
	if useDTLS13RecordHeader {
		record = c.appendDTLS13RecordHeader(record, seq, len(data))
		headerHasLength = !c.config.DTLSRecordHeaderOmitLength
	} else {
		record = append(record, byte(typ))
		record = append(record, byte(vers>>8))
		record = append(record, byte(vers))
		// DTLS records include an explicit sequence number.
		record = append(record, seq...)
		record = append(record, byte(len(data)>>8))
		record = append(record, byte(len(data)))
	}

	recordHeaderLen := len(record)
	record, err = c.out.encrypt(record, data, typ, recordHeaderLen, headerHasLength, seq)
	if err != nil {
		return
	}

	// Encrypt the sequence number.
	if useDTLS13RecordHeader && !c.config.Bugs.NullAllCiphers {
		sample := record[recordHeaderLen:]
		mask := c.out.recordNumberEncrypter.generateMask(sample)
		seqLen := 2
		if c.config.DTLSUseShortSeqNums {
			seqLen = 1
		}
		// The sequence number starts at index 1 in the record header.
		xorSlice(record[1:1+seqLen], mask)
	}

	// Flush the current pending packet if necessary.
	if !mustPack && len(record)+len(c.pendingPacket) > c.config.Bugs.PackHandshakeRecords {
		err = c.dtlsFlushPacket()
		if err != nil {
			return
		}
	}

	// Add the record to the pending packet.
	c.pendingPacket = append(c.pendingPacket, record...)
	if c.config.DTLSRecordHeaderOmitLength {
		if c.config.Bugs.SplitAndPackAppData {
			panic("incompatible config")
		}
		err = c.dtlsFlushPacket()
		if err != nil {
			return
		}
	}
	n = len(data)
	return
}

func (c *Conn) dtlsFlushPacket() error {
	if len(c.pendingPacket) == 0 {
		return nil
	}
	_, err := c.conn.Write(c.pendingPacket)
	c.pendingPacket = nil
	return err
}

func (c *Conn) dtlsDoReadHandshake() ([]byte, error) {
	// Assemble a full handshake message.  For test purposes, this
	// implementation assumes fragments arrive in order. It may
	// need to be cleverer if we ever test BoringSSL's retransmit
	// behavior.
	for len(c.handMsg) < 4+c.handMsgLen {
		// Get a new handshake record if the previous has been
		// exhausted.
		if c.hand.Len() == 0 {
			if err := c.in.err; err != nil {
				return nil, err
			}
			if err := c.readRecord(recordTypeHandshake); err != nil {
				return nil, err
			}
		}

		// Read the next fragment. It must fit entirely within
		// the record.
		if c.hand.Len() < 12 {
			return nil, errors.New("dtls: bad handshake record")
		}
		header := c.hand.Next(12)
		fragN := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		fragSeq := uint16(header[4])<<8 | uint16(header[5])
		fragOff := int(header[6])<<16 | int(header[7])<<8 | int(header[8])
		fragLen := int(header[9])<<16 | int(header[10])<<8 | int(header[11])

		if c.hand.Len() < fragLen {
			return nil, errors.New("dtls: fragment length too long")
		}
		fragment := c.hand.Next(fragLen)

		// Check it's a fragment for the right message.
		if fragSeq != c.recvHandshakeSeq {
			return nil, errors.New("dtls: bad handshake sequence number")
		}

		// Check that the length is consistent.
		if c.handMsg == nil {
			c.handMsgLen = fragN
			if c.handMsgLen > maxHandshake {
				return nil, c.in.setErrorLocked(c.sendAlert(alertInternalError))
			}
			// Start with the TLS handshake header,
			// without the DTLS bits.
			c.handMsg = slices.Clone(header[:4])
		} else if fragN != c.handMsgLen {
			return nil, errors.New("dtls: bad handshake length")
		}

		// Add the fragment to the pending message.
		if 4+fragOff != len(c.handMsg) {
			return nil, errors.New("dtls: bad fragment offset")
		}
		if fragOff+fragLen > c.handMsgLen {
			return nil, errors.New("dtls: bad fragment length")
		}
		c.handMsg = append(c.handMsg, fragment...)
	}
	c.recvHandshakeSeq++
	ret := c.handMsg
	c.handMsg, c.handMsgLen = nil, 0
	return ret, nil
}

// DTLSServer returns a new DTLS server side connection
// using conn as the underlying transport.
// The configuration config must be non-nil and must have
// at least one certificate.
func DTLSServer(conn net.Conn, config *Config) *Conn {
	c := &Conn{config: config, isDTLS: true, conn: conn}
	c.init()
	return c
}

// DTLSClient returns a new DTLS client side connection
// using conn as the underlying transport.
// The config cannot be nil: users must set either ServerHostname or
// InsecureSkipVerify in the config.
func DTLSClient(conn net.Conn, config *Config) *Conn {
	c := &Conn{config: config, isClient: true, isDTLS: true, conn: conn}
	c.init()
	return c
}
