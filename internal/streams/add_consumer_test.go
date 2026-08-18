package streams

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// stubProducer is a minimal core.Producer for unit-testing the matcher.
type stubProducer struct {
	medias []*core.Media
}

func (s *stubProducer) GetMedias() []*core.Media { return s.medias }
func (s *stubProducer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return core.NewReceiver(media, codec), nil
}
func (s *stubProducer) Start() error { return nil }
func (s *stubProducer) Stop() error  { return nil }

// dahuaBackchannel builds a Dahua-like camera: it sends us PCMA (recvonly) and
// accepts many codecs back on its sendonly/backchannel list.
func dahuaProducer(backchannelCodecs []*core.Codec) *Producer {
	return &Producer{conn: &stubProducer{medias: []*core.Media{
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000}},
		},
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs:    backchannelCodecs,
		},
	}}}
}

func TestMatchPreferredBackchannel(t *testing.T) {
	// Camera sends us PCMA (apples) and its long backchannel list also includes
	// PCMA, so the matcher must hard-pick PCMA — not the first match (AAC).
	backchannel := []*core.Codec{
		{Name: core.CodecAAC, ClockRate: 16000},
		{Name: core.CodecPCMU, ClockRate: 8000},
		{Name: core.CodecPCMA, ClockRate: 8000},
	}
	producer := dahuaProducer(backchannel)

	sendonly := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: backchannel}
	consRecv := &core.Media{
		Kind: core.KindAudio, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{
			{Name: core.CodecOpus, ClockRate: 48000},
			{Name: core.CodecPCMA, ClockRate: 8000},
			{Name: core.CodecPCMU, ClockRate: 8000},
		},
	}

	pc, cc := matchPreferredBackchannel(producer, sendonly, consRecv)
	require.NotNil(t, pc)
	require.Equal(t, core.CodecPCMA, pc.Name)
	require.Equal(t, uint32(8000), pc.ClockRate)
	require.NotNil(t, cc)
	require.Equal(t, core.CodecPCMA, cc.Name)
}

func TestMatchPreferredBackchannelNotOnList(t *testing.T) {
	// Camera sends us PCMA, but its backchannel list does NOT include PCMA.
	// Nothing should be forced — the caller falls back to the default match.
	backchannel := []*core.Codec{
		{Name: core.CodecAAC, ClockRate: 16000},
		{Name: core.CodecPCMU, ClockRate: 8000},
	}
	producer := dahuaProducer(backchannel)

	sendonly := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: backchannel}
	consRecv := &core.Media{
		Kind: core.KindAudio, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}},
	}

	pc, cc := matchPreferredBackchannel(producer, sendonly, consRecv)
	require.Nil(t, pc)
	require.Nil(t, cc)
}

func TestMatchPreferredBackchannelConsumerUnsupported(t *testing.T) {
	// Camera sends us PCMA and PCMA IS on its backchannel list, but the consumer
	// (caller) can't send PCMA. We must not force a codec the caller can't
	// produce — fall back to the default match.
	backchannel := []*core.Codec{
		{Name: core.CodecPCMA, ClockRate: 8000},
		{Name: core.CodecPCMU, ClockRate: 8000},
	}
	producer := dahuaProducer(backchannel)

	sendonly := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: backchannel}
	consRecv := &core.Media{
		Kind: core.KindAudio, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}},
	}

	pc, cc := matchPreferredBackchannel(producer, sendonly, consRecv)
	require.Nil(t, pc)
	require.Nil(t, cc)
}
