// Copyright (C) 2023  mieru authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package protocolv2

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/enfein/mieru/pkg/congestion"
	"github.com/enfein/mieru/pkg/log"
	"github.com/enfein/mieru/pkg/mathext"
	"github.com/enfein/mieru/pkg/metrics"
	"github.com/enfein/mieru/pkg/netutil"
	"github.com/enfein/mieru/pkg/stderror"
)

const (
	segmentTreeCapacity = 4096
	segmentChanCapacity = 256
	minWindowSize       = 32
	maxWindowSize       = 4096
	segmentPollInterval = 10 * time.Millisecond
)

type sessionState int

const (
	sessionInit sessionState = iota
	sessionAttached
	sessionOpening
	sessionEstablished
	sessionClosing
	sessionClosed
)

type Session struct {
	conn Underlay // underlay connection

	id          uint32        // session ID number
	isClient    bool          // if this session is owned by client
	mtu         int           // L2 maxinum transmission unit
	state       sessionState  // session state
	ready       chan struct{} // indicate the session is ready to use
	established chan struct{} // indicate the session handshake is completed
	done        chan struct{} // indicate the session is complete

	sendQueue *segmentTree  // segments waiting to send
	sendBuf   *segmentTree  // segments sent but not acknowledged
	recvBuf   *segmentTree  // segments received but acknowledge is not sent
	recvQueue *segmentTree  // segments waiting to be read by application
	recvChan  chan *segment // channel to receive segment from underlay

	nextSeq   uint32 // next sequence number to send a segment
	nextRecv  uint32 // next sequence number to receive
	unackSeq  uint32 // unacknowledged sequence number
	unreadBuf []byte // payload removed from the recvQueue that haven't been read by application

	rttStat       *congestion.RTTStats
	sendAlgorithm *congestion.CubicSendAlgorithm

	wg    sync.WaitGroup
	rLock sync.Mutex
	wLock sync.Mutex
}

// Session must implement net.Conn interface.
var _ net.Conn = &Session{}

// NewSession creates a new session.
func NewSession(id uint32, isClient bool, mtu int) *Session {
	rttStat := congestion.NewRTTStats()
	rttStat.SetMaxAckDelay(2 * segmentPollInterval)
	rttStat.SetRTOMultiplier(1.5)
	return &Session{
		conn:          nil,
		id:            id,
		isClient:      isClient,
		mtu:           mtu,
		state:         sessionInit,
		ready:         make(chan struct{}),
		established:   make(chan struct{}),
		done:          make(chan struct{}),
		sendQueue:     newSegmentTree(segmentTreeCapacity),
		sendBuf:       newSegmentTree(segmentTreeCapacity),
		recvBuf:       newSegmentTree(segmentTreeCapacity),
		recvQueue:     newSegmentTree(segmentTreeCapacity),
		recvChan:      make(chan *segment, segmentChanCapacity),
		nextSeq:       0,
		nextRecv:      0,
		unackSeq:      0,
		rttStat:       rttStat,
		sendAlgorithm: congestion.NewCubicSendAlgorithm(minWindowSize, maxWindowSize),
	}
}

func (s *Session) String() string {
	if s.conn == nil {
		return fmt.Sprintf("Session{%d}", s.id)
	}
	return fmt.Sprintf("Session{%d - %v - %v}", s.id, s.LocalAddr(), s.RemoteAddr())
}

// Read lets a user to read data from receive queue.
// The data boundary is preserved, i.e. no fragment read.
func (s *Session) Read(b []byte) (n int, err error) {
	if s.state < sessionAttached {
		return 0, fmt.Errorf("%v is not ready for Write()", s)
	}
	if s.state >= sessionClosed {
		return 0, io.ErrClosedPipe
	}
	s.rLock.Lock()
	defer s.rLock.Unlock()
	if log.IsLevelEnabled(log.TraceLevel) {
		log.Tracef("%v trying to read %d bytes", s, len(b))
	}

	// There are some remaining data that application
	// failed to read last time due to short buffer.
	if len(s.unreadBuf) > 0 {
		if len(b) < len(s.unreadBuf) {
			return 0, io.ErrShortBuffer
		}
		n = copy(b, s.unreadBuf)
		s.unreadBuf = nil
		metrics.InBytes.Add(int64(n))
		return n, nil
	}

	// Read all the fragments of the original message.
	for {
		seg, ok := s.recvQueue.DeleteMinBlocking()
		if !ok {
			// recvQueue is dead.
			return 0, io.EOF
		}

		if s.isClient && seg.metadata.Protocol() == openSessionResponse && (s.state == sessionAttached || s.state == sessionOpening) {
			s.forwardStateTo(sessionEstablished)
			close(s.established)
			// fallthrough
		}

		if len(s.unreadBuf) == 0 {
			s.unreadBuf = seg.payload
		} else {
			s.unreadBuf = append(s.unreadBuf, seg.payload...)
		}

		fragment, err := seg.Fragment()
		if err != nil {
			return 0, fmt.Errorf("Fragment() failed: %w", err)
		}
		if fragment == 0 {
			break
		}
	}

	if len(b) < len(s.unreadBuf) {
		return 0, io.ErrShortBuffer
	}
	n = copy(b, s.unreadBuf)
	s.unreadBuf = nil
	metrics.InBytes.Add(int64(n))
	return n, nil
}

// Write stores the data to send queue.
func (s *Session) Write(b []byte) (n int, err error) {
	if len(b) > MaxPDU {
		return 0, io.ErrShortWrite
	}
	if s.state < sessionAttached {
		return 0, fmt.Errorf("%v is not ready for Write()", s)
	}
	if s.state >= sessionClosed {
		return 0, io.ErrClosedPipe
	}
	s.wLock.Lock()
	defer s.wLock.Unlock()

	if s.state == sessionAttached {
		if s.isClient {
			// Send open session request.
			seg := &segment{
				metadata: &sessionStruct{
					baseStruct: baseStruct{
						protocol: openSessionRequest,
					},
					sessionID:  s.id,
					seq:        s.nextSeq,
					statusCode: 0,
					payloadLen: 0,
				},
			}
			s.nextSeq++
			if len(b) <= maxSessionOpenPDU {
				seg.metadata.(*sessionStruct).payloadLen = uint16(len(b))
				seg.payload = b
			}
			if log.IsLevelEnabled(log.TraceLevel) {
				log.Tracef("%v writing %d bytes with open session request", s, len(seg.payload))
			}
			s.sendQueue.InsertBlocking(seg)
			s.forwardStateTo(sessionOpening)
			if len(seg.payload) > 0 {
				return len(seg.payload), nil
			}
		} else {
			// Send open session response.
			seg := &segment{
				metadata: &sessionStruct{
					baseStruct: baseStruct{
						protocol: openSessionResponse,
					},
					sessionID:  s.id,
					seq:        s.nextSeq,
					statusCode: 0,
					payloadLen: 0,
				},
			}
			s.nextSeq++
			if len(b) <= maxSessionOpenPDU {
				seg.metadata.(*sessionStruct).payloadLen = uint16(len(b))
				seg.payload = b
			}
			if log.IsLevelEnabled(log.TraceLevel) {
				log.Tracef("%v writing %d bytes with open session response", s, len(seg.payload))
			}
			s.sendQueue.InsertBlocking(seg)
			s.forwardStateTo(sessionEstablished)
			if len(seg.payload) > 0 {
				return len(seg.payload), nil
			}
		}
	}

	nFragment := 1
	fragmentSize := MaxFragmentSize(s.mtu, s.conn.IPVersion(), s.conn.TransportProtocol())
	if len(b) > fragmentSize {
		nFragment = (len(b)-1)/fragmentSize + 1
	}
	if log.IsLevelEnabled(log.TraceLevel) {
		log.Tracef("%v writing %d bytes with %d fragments", s, len(b), nFragment)
	}

	ptr := b
	for i := nFragment - 1; i >= 0; i-- {
		var protocol uint8
		if s.isClient {
			protocol = dataClientToServer
		} else {
			protocol = dataServerToClient
		}
		partLen := mathext.Min(fragmentSize, len(ptr))
		part := ptr[:partLen]
		seg := &segment{
			metadata: &dataAckStruct{
				baseStruct: baseStruct{
					protocol: protocol,
				},
				sessionID:  s.id,
				seq:        s.nextSeq,
				unAckSeq:   s.unackSeq,
				windowSize: uint16(s.recvBuf.Remaining()),
				fragment:   uint8(i),
				payloadLen: uint16(partLen),
			},
			payload: part,
		}
		s.nextSeq++
		s.sendQueue.InsertBlocking(seg)
		ptr = ptr[partLen:]
	}
	n = len(b)
	metrics.OutBytes.Add(int64(n))
	return n, nil
}

// Close actively terminates the session. If the session is terminated by the
// other party, underlay is responsible to terminate the session at our end.
func (s *Session) Close() error {
	select {
	case <-s.done:
		s.forwardStateTo(sessionClosed)
		log.Debugf("%v is already closed", s)
		return nil
	default:
	}

	log.Debugf("Closing %v", s)
	s.rLock.Lock()
	s.wLock.Lock()
	defer s.rLock.Unlock()
	defer s.wLock.Unlock()

	s.forwardStateTo(sessionClosing)
	seg := &segment{
		metadata: &sessionStruct{
			baseStruct: baseStruct{
				protocol: closeSessionRequest,
			},
			sessionID:  s.id,
			seq:        s.nextSeq,
			statusCode: 0,
			payloadLen: 0,
		},
	}
	s.nextSeq++
	s.sendQueue.InsertBlocking(seg)
	<-s.done
	s.forwardStateTo(sessionClosed)
	return nil
}

func (s *Session) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Session) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

func (s *Session) SetDeadline(t time.Time) error {
	return stderror.ErrUnsupported
}

func (s *Session) SetReadDeadline(t time.Time) error {
	return stderror.ErrUnsupported
}

func (s *Session) SetWriteDeadline(t time.Time) error {
	return stderror.ErrUnsupported
}

func (s *Session) forwardStateTo(new sessionState) {
	if new < s.state {
		panic(fmt.Sprintf("Can't move state back from %v to %v", s.state, new))
	}
	s.state = new
}

func (s *Session) runInputLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		case seg := <-s.recvChan:
			if err := s.input(seg); err != nil {
				return fmt.Errorf("input() failed: %v", err)
			}
		}
	}
}

func (s *Session) runOutputLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		default:
			switch s.conn.TransportProtocol() {
			case netutil.TCPTransport:
				for {
					seg, ok := s.sendQueue.DeleteMin()
					if !ok {
						time.Sleep(segmentPollInterval)
						break
					}
					if err := s.output(seg); err != nil {
						return fmt.Errorf("output() failed: %v", err)
					}
				}
			case netutil.UDPTransport:
				// Delete segments that are acked in sendBuf.
				// Refill sendBuf with segments from sendQueue.
				// Flush sendBuf.
			default:
				return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
			}
		}
	}
}

// input reads incoming packets from network and assemble
// them in the receive buffer and receive queue.
func (s *Session) input(seg *segment) error {
	if log.IsLevelEnabled(log.TraceLevel) {
		log.Tracef("%v input %v", s, seg)
	}
	protocol := seg.Protocol()
	if s.isClient {
		if protocol != openSessionResponse && protocol != dataServerToClient && protocol != ackServerToClient && protocol != closeSessionRequest && protocol != closeSessionResponse {
			return stderror.ErrInvalidArgument
		}
	} else {
		if protocol != openSessionRequest && protocol != dataClientToServer && protocol != ackClientToServer && protocol != closeSessionRequest && protocol != closeSessionResponse {
			return stderror.ErrInvalidArgument
		}
	}
	if protocol == openSessionRequest || protocol == openSessionResponse || protocol == dataServerToClient || protocol == dataClientToServer {
		return s.inputData(seg)
	} else if protocol == ackServerToClient || protocol == ackClientToServer {
		return s.inputAck(seg)
	} else if protocol == closeSessionRequest || protocol == closeSessionResponse {
		return s.inputClose(seg)
	}
	return nil
}

func (s *Session) inputData(seg *segment) error {
	switch s.conn.TransportProtocol() {
	case netutil.TCPTransport:
		// Deliver the segment directly to recvQueue.
		s.recvQueue.InsertBlocking(seg)
		return nil
	case netutil.UDPTransport:
		// Deliver the segment to recvBuf.
		s.recvBuf.InsertBlocking(seg)
		// Move recvBuf to recvQueue.
		for {
			seg, deleted := s.recvBuf.DeleteMinIf(func(iter *segment) bool {
				seq, err := iter.Seq()
				if err != nil {
					panic(fmt.Sprintf("%v get segment sequence number failed: %v", s, err))
				}
				if seq <= s.nextRecv {
					return true
				}
				return false
			})
			if seg == nil || !deleted {
				return nil
			}
			seq, err := seg.Seq()
			if err != nil {
				panic(fmt.Sprintf("%v get segment sequence number failed: %v", s, err))
			}
			if seq == s.nextRecv {
				s.recvQueue.InsertBlocking(seg)
				s.nextRecv++
			}
		}
	default:
		return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
	}
}

func (s *Session) inputAck(seg *segment) error {
	switch s.conn.TransportProtocol() {
	case netutil.TCPTransport:
		// Do nothing when receive ACK from TCP protocol.
		return nil
	case netutil.UDPTransport:
		// Delete all previous acknowledged segments.
		das := seg.metadata.(*dataAckStruct)
		unAckSeq := das.unAckSeq
		for {
			_, deleted := s.sendBuf.DeleteMinIf(func(iter *segment) bool {
				seq, err := iter.Seq()
				if err != nil {
					panic(fmt.Sprintf("%v get segment sequence number failed: %v", s, err))
				}
				if seq < unAckSeq {
					return true
				}
				return false
			})
			if !deleted {
				break
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
	}
}

func (s *Session) inputClose(seg *segment) error {
	if seg.metadata.Protocol() == closeSessionRequest {
		// Send close session response.
		seg2 := &segment{
			metadata: &sessionStruct{
				baseStruct: baseStruct{
					protocol: closeSessionResponse,
				},
				sessionID:  s.id,
				seq:        s.nextSeq,
				statusCode: 0,
				payloadLen: 0,
			},
		}
		s.nextSeq++
		// The response will not retry if it is not delivered.
		if err := s.output(seg2); err != nil {
			return fmt.Errorf("output() failed: %v", err)
		}
		// Immediately shutdown event loop.
		if log.IsLevelEnabled(log.TraceLevel) {
			log.Tracef("Shutdown %v", s)
		}
		close(s.done)
		s.forwardStateTo(sessionClosed)
	} else if seg.metadata.Protocol() == closeSessionResponse {
		// Immediately shutdown event loop.
		if log.IsLevelEnabled(log.TraceLevel) {
			log.Tracef("Shutdown %v", s)
		}
		close(s.done)
		s.forwardStateTo(sessionClosed)
	}
	return nil
}

func (s *Session) output(seg *segment) error {
	if log.IsLevelEnabled(log.TraceLevel) {
		log.Tracef("%v output %v", s, seg)
	}
	switch s.conn.TransportProtocol() {
	case netutil.TCPTransport:
		if err := s.conn.(*TCPUnderlay).writeOneSegment(seg); err != nil {
			return fmt.Errorf("TCPUnderlay.writeOneSegment() failed: %v", err)
		}
	default:
		return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
	}
	return nil
}
