package rtp

import (
	"fmt"
	"net"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

// RTP implements a raw RTP audio endpoint that is both a Consumer (camera to caller)
// and a Producer (caller to camera backchannel). No transcoding is performed.
//
// GetMedias() advertises both sendonly (caller receives camera audio) and recvonly
// (caller sends audio back). The stream matcher pairs each direction independently.
//
// The endpoint listens on two sockets: RTP (port) and RTCP (port+1). Both feed into
// the same activity tracking so the session stays alive as long as the caller sends
// any packets. RTCP Receiver Reports are generated in response to incoming SRs.
type RTP struct {
	core.Connection

	mu         sync.Mutex
	remoteAddr *net.UDPAddr
	localConn  *net.UDPConn
	rtcpConn   *net.UDPConn

	// when true (SIP two-way), the backchannel (recvonly) media is not advertised
	// in GetMedias, so the stream matcher never pairs a backchannel sender onto a
	// watch conn. Backchannel is instead driven explicitly into a dedicated rtsp
	// connection via BackchannelReceiver().
	noPoolRecvonly bool

	// receiver is the backchannel track (caller to camera)
	receiver *core.Receiver

	// payloadType is set from the codec passed to AddTrack / GetTrack
	payloadType byte

	// seqNum / ssrc for outgoing packets
	seqNum uint16
	ssrc   uint32

	// rtcpPacketCount for periodic RR generation
	rtcpPacketCount int

	// OnActivity is called on every valid RTP/RTCP packet from the remote.
	// Used for session keepalive.
	OnActivity func()
}

// NewRTP creates an audio-only RTP endpoint.
//   - remote: target address in "host:port" form
//   - localPort: UDP port to listen on for incoming RTP (RTCP uses port+1)
func NewRTP(remote string, localPort int) (*RTP, error) {
	remoteAddr, err := net.ResolveUDPAddr("udp", remote)
	if err != nil {
		return nil, fmt.Errorf("invalid remote address: %w", err)
	}

	localConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on port %d: %w", localPort, err)
	}

	var rtcpConn *net.UDPConn
	if localPort+1 > 0 && localPort+1 <= 65535 {
		rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{Port: localPort + 1})
		if err != nil {
			rtcpConn = nil
		}
	}

	r := &RTP{
		remoteAddr: remoteAddr,
		localConn:  localConn,
		rtcpConn:   rtcpConn,
		ssrc:       1,
	}

	r.SetProtocol("rtp")
	r.SetRemoteAddr(remote)

	go r.readLoop()
	if rtcpConn != nil {
		go r.rtcpReadLoop()
	}

	return r, nil
}

// GetMedias returns media descriptions for the stream matcher.
// Audio: sendonly (caller receives). If the pool backchannel is enabled, also
// recvonly (caller sends back) is advertised so the matcher can pair it.
func (r *RTP) GetMedias() []*core.Media {
	r.mu.Lock()
	noRecv := r.noPoolRecvonly
	r.mu.Unlock()

	medias := []*core.Media{
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs:    audioCodecs(),
		},
	}
	if !noRecv {
		medias = append(medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    audioCodecs(),
		})
	}
	return medias
}

// DisablePoolRecvonly stops the endpoint from advertising its backchannel
// (recvonly) media to the stream matcher. Used by the SIP module so a caller→
// camera backchannel is never paired onto a watch conn (which can't send);
// instead it is driven explicitly via BackchannelReceiver().
func (r *RTP) DisablePoolRecvonly() {
	r.mu.Lock()
	r.noPoolRecvonly = true
	r.mu.Unlock()
}

// audioCodecs returns the SIP-compatible audio codecs with standard payload types.
func audioCodecs() []*core.Codec {
	return []*core.Codec{
		{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111},
		{Name: core.CodecG722, ClockRate: 8000, PayloadType: 9},
		{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8},
		{Name: core.CodecPCMU, ClockRate: 8000, PayloadType: 0},
	}
}

// AddTrack is called when the camera produces an audio track (sendonly direction).
func (r *RTP) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.payloadType = codec.PayloadType

	sender := core.NewSender(media, codec)
	sender.WithParent(track)
	sender.Output = func(packet *core.Packet) {
		r.sendToRemote(packet)
	}
	sender.Start()

	r.Senders = append(r.Senders, sender)
	return nil
}

// GetTrack is called when the stream layer needs a backchannel receiver.
// The codec's PayloadType is only relevant for the caller->camera direction;
// outgoing (camera->caller) packets keep their own PT from AddTrack.
func (r *RTP) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.receiver != nil {
		return r.receiver, nil
	}

	r.receiver = core.NewReceiver(media, codec)

	// FIX: Register the receiver with the Connection so Stop() will close it
	r.Receivers = append(r.Receivers, r.receiver)

	return r.receiver, nil
}

// BackchannelReceiver returns the shared backchannel (caller→camera) receiver,
// creating and registering it lazily the first time it is requested. It mirrors
// GetTrack but is meant to be called directly by the SIP module so the receiver
// can be wired into a dedicated rtsp backchannel connection (instead of relying
// on the stream matcher — which would attach it to a watch conn that can't send).
func (r *RTP) BackchannelReceiver(media *core.Media, codec *core.Codec) *core.Receiver {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.receiver != nil {
		return r.receiver
	}

	r.receiver = core.NewReceiver(media, codec)
	r.Receivers = append(r.Receivers, r.receiver)

	return r.receiver
}

func (r *RTP) Start() error {
	return nil
}

func (r *RTP) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.localConn != nil {
		r.localConn.Close()
		r.localConn = nil
	}
	if r.rtcpConn != nil {
		r.rtcpConn.Close()
		r.rtcpConn = nil
	}

	return r.Connection.Stop()
}

// rtcpReadLoop reads incoming RTCP packets from the RTCP socket.
func (r *RTP) rtcpReadLoop() {
	buf := make([]byte, 1500)
	for {
		r.mu.Lock()
		conn := r.rtcpConn
		r.mu.Unlock()

		if conn == nil {
			return
		}

		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		if n < 4 {
			continue
		}

		r.handleRTCP(buf[:n])

		if r.OnActivity != nil {
			r.OnActivity()
		}
	}
}

// sendToRemote sends an RTP packet to the remote endpoint.
func (r *RTP) sendToRemote(packet *core.Packet) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.localConn == nil {
		return
	}

	r.seqNum++

	// Marker bit is zeroed: for G.711 (PCMA/PCMU) RFC 3551 requires M=0
	// in every packet. Passing through the camera's marker can confuse
	// the receiver's jitter buffer and trigger false PLC.
	header := rtp.Header{
		Version:        2,
		Marker:         false,
		PayloadType:    r.payloadType,
		SequenceNumber: r.seqNum,
		Timestamp:      packet.Timestamp,
		SSRC:           r.ssrc,
	}

	pkt := &rtp.Packet{
		Header:  header,
		Payload: packet.Payload,
	}

	data, err := pkt.Marshal()
	if err != nil {
		return
	}

	r.localConn.WriteToUDP(data, r.remoteAddr)
}

// readLoop reads incoming RTP packets from the remote endpoint.
func (r *RTP) readLoop() {
	buf := make([]byte, 1500)
	for {
		n, _, err := r.localConn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		if n < 2 {
			continue
		}

		// RTCP always has V=2 and PT >= 200 (SR=200, RR=201, SDES=202, etc).
		// RTP can have byte1 >= 200 when the marker bit is set on high PTs
		// (e.g. Opus PT=111 + M=1 → 239), so we must verify V=2 first.
		if buf[0]&0xC0 == 0x80 && buf[1] >= 200 {
			r.handleRTCP(buf[:n])
			if r.OnActivity != nil {
				r.OnActivity()
			}
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		// Every valid RTP packet from the caller resets the activity timer.
		if r.OnActivity != nil {
			r.OnActivity()
		}

		r.mu.Lock()
		receiver := r.receiver
		r.mu.Unlock()

		if receiver != nil {
			packet := &core.Packet{
				Header:  pkt.Header,
				Payload: pkt.Payload,
			}
			receiver.Input(packet)
		}
	}
}

// handleRTCP parses an RTCP compound packet and responds to SR with RR.
func (r *RTP) handleRTCP(data []byte) {
	if len(data) < 4 {
		return
	}

	offset := 0
	for offset+4 <= len(data) {
		ver := (data[offset] >> 6) & 0x03
		pt := data[offset+1]
		length := int(data[offset+2])<<8 | int(data[offset+3])
		length = (length + 1) * 4

		if ver != 2 || offset+length > len(data) {
			break
		}

		if pt == 200 && offset+28 <= len(data) {
			ssrc := uint32(data[offset+4])<<24 | uint32(data[offset+5])<<16 | uint32(data[offset+6])<<8 | uint32(data[offset+7])
			rtpTS := uint32(data[offset+16])<<24 | uint32(data[offset+17])<<16 | uint32(data[offset+18])<<8 | uint32(data[offset+19])
			r.sendRTCPRRWithSSRC(ssrc, rtpTS)
		}

		offset += length
	}
}

func (r *RTP) sendRTCPRR() {
	r.sendRTCPRRWithSSRC(0, 0)
}

// sendRTCPRRWithSSRC sends a Receiver Report for a specific sender.
// RTCP must go to the remote RTCP port (RTP port + 1), not the RTP port.
func (r *RTP) sendRTCPRRWithSSRC(senderSSRC uint32, lastRTPTS uint32) {
	if r.localConn == nil || r.remoteAddr == nil {
		return
	}

	rr := make([]byte, 32)

	rr[0] = 0x81  // V=2, RC=1
	rr[1] = 201   // PT=201 (RR)
	rr[2] = 0
	rr[3] = 7

	rr[4] = byte(r.ssrc >> 24)
	rr[5] = byte(r.ssrc >> 16)
	rr[6] = byte(r.ssrc >> 8)
	rr[7] = byte(r.ssrc)

	rr[8] = byte(senderSSRC >> 24)
	rr[9] = byte(senderSSRC >> 16)
	rr[10] = byte(senderSSRC >> 8)
	rr[11] = byte(senderSSRC)

	rr[12] = 0
	rr[13] = 0
	rr[14] = 0
	rr[15] = 0

	rr[16] = byte(lastRTPTS >> 24)
	rr[17] = byte(lastRTPTS >> 16)
	rr[18] = byte(lastRTPTS >> 8)
	rr[19] = byte(lastRTPTS)

	rtcpAddr := &net.UDPAddr{
		IP:   r.remoteAddr.IP,
		Port: r.remoteAddr.Port + 1,
	}
	r.localConn.WriteToUDP(rr, rtcpAddr)
}

// RemoteIP returns the remote IP address as a string, or empty if not set.
func (r *RTP) RemoteIP() string {
	if r.remoteAddr == nil {
		return ""
	}
	return r.remoteAddr.IP.String()
}
