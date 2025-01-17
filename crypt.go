/*
 Copyright (c) Facebook, Inc. and its affiliates.

 This source code is licensed under the MIT license found in the
 LICENSE file in the root directory of this source tree.
*/

package tacquito

import (
	"bufio"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/facebookincubator/tacquito/proxy"
)

/* crypt calculates the psuedo-random pad for a TACACs+ packet and performs xor ops
   See https://datatracker.ietf.org/doc/html/rfc8907#section-4.5 for more info

   The packet body is obfuscated by XOR-ing it byte-wise with a pseudo-random pad.
      ENCRYPTED {data} = data ^ pseudo_pad

   The packet body can then be de-obfuscated by XOR-ing it byte-wise with a pseudo random pad.
      data = ENCRYPTED {data} ^ pseudo_pad

   The pad is generated by concatenating a series of MD5 hashes (each 16 bytes long) and truncating it to the length of the input data.
      pseudo_pad = {MD5_1 [,MD5_2 [ ... ,MD5_n]]} truncated to len(data)

   The first MD5 hash is generated by concatenating the session_id, the
   secret key, the version number and the sequence number and then
   running MD5 over that stream.

   Subsequent hashes are generated by using the same input stream, but
   concatenating the previous hash value at the end of the input stream.

   MD5_1 = MD5{session_id, key, version, seq_no} MD5_2 = MD5{session_id, key, version, seq_no, MD5_1} ....  MD5_n = MD5{session_id, key, version, seq_no, MD5_n-1}

   WARNING: Per the RFC, this is not 'real' encryption. This algorithm does not meet modern standards, but like The Mandalorian says, "This Is The Way".
*/
func crypt(secret []byte, p *Packet) error {
	if p.Header.Flags.Has(UnencryptedFlag) {
		return nil
	}

	sessionID, err := p.Header.SessionID.MarshalBinary()
	if err != nil {
		return err
	}

	version, err := p.Header.Version.MarshalBinary()
	if err != nil {
		return err
	}

	lastHash := make([]byte, 0, 16)
	pad := make([]byte, 0, p.Header.Length)
	hash := md5.New()
	for len(pad) < int(p.Header.Length) {
		hash.Reset()
		hash.Write(sessionID)
		hash.Write(secret)
		hash.Write(version)
		hash.Write([]byte{byte(p.Header.SeqNo)})
		hash.Write(lastHash)

		lastHash = hash.Sum(nil)

		pad = append(pad, lastHash[:]...)

		// truncate to length of body
		if len(pad) > int(p.Header.Length) {
			pad = pad[:int(p.Header.Length)]
		}
	}

	// perform xor ops
	for i, b := range p.Body {
		p.Body[i] = b ^ pad[i]
	}
	return nil
}

// newCrypter makes a new crypter
func newCrypter(secret []byte, c net.Conn, proxy bool) *crypter {
	return &crypter{secret: secret, Conn: c, Reader: bufio.NewReaderSize(c, 107), proxy: proxy}
}

// crypter wraps the net.Conn and performs reads and writes and crypt ops
// if the incoming packet is not crypted via the no crypt flag, nothing will happen
// to the underlying bytes.  However, if that flag is missing and the keys mismatch,
// the corresponding response from the server will appear to be malformed to the client
// since we'll still send a crypted reply for the secret the client should be using.
type crypter struct {
	net.Conn
	*bufio.Reader

	// secret is the tacacs psk used in crypt ops
	secret []byte
	// proxy if set, will strip the ha-proxy style ascii header
	proxy bool
}

// read will read a packet from the underlying net.Conn and decyrpt it
func (c *crypter) read() (*Packet, error) {
	// strip proxy header and record metrics
	if c.proxy {
		line, err := c.ReadBytes('\000') // octal null byte
		if err != nil {
			if err == io.EOF {
				return nil, err
			}
			crypterReadError.Inc()
			return nil, fmt.Errorf("unable to read header proxy line; %w", err)
		}
		p := proxy.NewHeader(c.LocalAddr(), c.RemoteAddr())
		if _, err := p.Write(line); err != nil {
			crypterReadError.Inc()
			return nil, fmt.Errorf("unable to extract proxy header; %w", err)
		}
		// TODO add metrics for reporting in next diff
	}

	// allocate a tacacs header
	h := make([]byte, MaxHeaderLength)
	if _, err := io.ReadFull(c.Reader, h); err != nil {
		if err != io.EOF {
			crypterReadError.Inc()
		}
		return nil, err
	}

	// read the length field from the bytes of the header to know how many more bytes we need to get
	s := int(binary.BigEndian.Uint32(h[8:]))
	if s > int(MaxBodyLength) {
		return nil, fmt.Errorf("max header length exceeded in crypt read, aborting")
	}
	b := make([]byte, s)
	if _, err := io.ReadFull(c.Reader, b); err != nil {
		crypterReadError.Inc()
		return nil, err
	}

	var p Packet
	err := Unmarshal(append(h, b...), &p)
	if err != nil {
		crypterUnmarshalError.Inc()
		return nil, err
	}
	// run crypt first before we look for bad secrets
	if err := crypt(c.secret, &p); err != nil {
		crypterCryptError.Inc()
		return nil, err
	}
	// if err is != nil, we hit a bug
	// if reply is != nil, we found a bad secret.
	// if both are non nil, we only inspect the error as that
	// is a higher error condition in the server than a bad secret is
	if reply, err := c.detectBadSecret(&p); err != nil {
		return nil, err
	} else if reply != nil {
		if _, err := c.write(reply); err != nil {
			return nil, fmt.Errorf("bad secret, crypt write fail for session [%v]: %v", p.Header.SessionID, err)
		}
		return nil, fmt.Errorf("bad secret detected for sessionID [%v]", p.Header.SessionID)
	}

	crypterRead.Inc()
	return &p, nil
}

// write takes a packet, marshals and crypts it
func (c *crypter) write(p *Packet) (int, error) {
	if p == nil {
		return 0, fmt.Errorf("handler error, packet cannot be nil")
	}
	if p.Body == nil {
		return 0, fmt.Errorf("handler error, packet.Body cannot be nil")
	}
	p.Header.Length = uint32(len(p.Body))
	if err := crypt(c.secret, p); err != nil {
		crypterCryptError.Inc()
		return 0, err
	}
	b, err := p.MarshalBinary()
	if err != nil {
		crypterMarshalError.Inc()
		return 0, err
	}

	n, err := c.Write(b)
	if err != nil {
		crypterWriteError.Inc()
		return 0, err
	}
	crypterWrite.Inc()
	return n, nil
}

// detectBadSecret is "a way" to detect a potential bad secret.  tacacs doesn't give
// us enough information to know what body to expect from a given header, so we
// have to go to great lengths to guess
func (c crypter) detectBadSecret(p *Packet) (*Packet, error) {
	if p.Header.Flags.Has(UnencryptedFlag) {
		return nil, nil
	}
	var badSecret *BadSecretErr
	switch p.Header.Type {
	case Authenticate:
		errCnt := 0
		var as AuthenStart
		if err := Unmarshal(p.Body, &as); errors.As(err, &badSecret) {
			errCnt++
		}
		var ac AuthenContinue
		if err := Unmarshal(p.Body, &ac); errors.As(err, &badSecret) {
			errCnt++
		}
		var ar AuthenReply
		if err := Unmarshal(p.Body, &ar); errors.As(err, &badSecret) {
			errCnt++
		}
		if errCnt == 3 {
			crypterBadSecret.Inc()
			// all packet types failed, most likley a bad secret
			return c.badSecretReply(p.Header)
		}
	case Authorize:
		errCnt := 0
		var ar AuthorRequest
		if err := Unmarshal(p.Body, &ar); errors.As(err, &badSecret) {
			errCnt++
		}
		var arr AuthorReply
		if err := Unmarshal(p.Body, &arr); errors.As(err, &badSecret) {
			errCnt++
		}
		if errCnt == 2 {
			crypterBadSecret.Inc()
			// all packet types failed, most likley a bad secret
			return c.badSecretReply(p.Header)
		}
	case Accounting:
		errCnt := 0
		var ar AcctRequest
		if err := Unmarshal(p.Body, &ar); errors.As(err, &badSecret) {
			errCnt++
		}
		var arr AcctReply
		if err := Unmarshal(p.Body, &arr); errors.As(err, &badSecret) {
			errCnt++
		}
		if errCnt == 2 {
			crypterBadSecret.Inc()
			// all packet types failed, most likley a bad secret
			return c.badSecretReply(p.Header)
		}
	}
	return nil, nil
}

func (c crypter) badSecretReply(h *Header) (*Packet, error) {
	var b []byte
	var err error
	switch h.Type {
	case Authenticate:
		b, err = NewAuthenReply(
			SetAuthenReplyStatus(AuthenStatusError),
			SetAuthenReplyServerMsg("bad secret"),
		).MarshalBinary()
		if err != nil {
			crypterMarshalError.Inc()
			return nil, err
		}
	case Authorize:
		b, err = NewAuthorReply(
			SetAuthorReplyStatus(AuthorStatusError),
			SetAuthorReplyServerMsg("bad secret"),
		).MarshalBinary()
		if err != nil {
			crypterMarshalError.Inc()
			return nil, err
		}
	case Accounting:
		b, err = NewAcctReply(
			SetAcctReplyStatus(AcctReplyStatusError),
			SetAcctReplyServerMsg("bad secret"),
		).MarshalBinary()
		if err != nil {
			crypterMarshalError.Inc()
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown header type [%v]", h.Type)
	}
	// reset some flags and state for this error reply.
	// under error conditions it can be common in the rfc to reset the sequence to 1
	// if the error is particularly egregious.  a bad secret seems like it fits and
	// the rfc is unclear for this particular condition on what to do
	h.SeqNo = SequenceNumber(1)
	p := NewPacket(
		SetPacketHeader(h),
		SetPacketBody(b),
	)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// BadSecretErr ...
type BadSecretErr struct {
	msg string
}

// NewBadSecretErr ...
func NewBadSecretErr(msg string) *BadSecretErr {
	return &BadSecretErr{msg: msg}
}

// Error ...
func (b BadSecretErr) Error() string {
	return b.msg
}
