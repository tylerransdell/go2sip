package streams

import (
	"errors"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

func (s *Stream) AddConsumer(cons core.Consumer) (err error) {
	// support for multiple simultaneous pending from different consumers
	consN := s.pending.Add(1) - 1

	var prodErrors = make([]error, len(s.producers))
	var prodMedias []*core.Media
	var prodStarts []*Producer

	// Step 1. Get consumer medias
	consMedias := cons.GetMedias()
	for _, consMedia := range consMedias {
		log.Trace().Msgf("[streams] check cons=%d media=%s", consN, consMedia)

	producers:
		for prodN, prod := range s.producers {
			// check for loop request, ex. `camera1: ffmpeg:camera1`
			if info, ok := cons.(core.Info); ok && prod.url == info.GetSource() {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d", consN, prodN)
				continue
			}

			if prodErrors[prodN] != nil {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d", consN, prodN)
				continue
			}

			if err = prod.Dial(); err != nil {
				log.Trace().Err(err).Msgf("[streams] dial cons=%d prod=%d", consN, prodN)
				prodErrors[prodN] = err
				continue
			}

			// Step 2. Get producer medias (not tracks yet)
			for _, prodMedia := range prod.GetMedias() {
				log.Trace().Msgf("[streams] check cons=%d prod=%d media=%s", consN, prodN, prodMedia)
				prodMedias = append(prodMedias, prodMedia)

				// Step 3. Match consumer/producer codecs list
				prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
				if prodCodec == nil {
					continue
				}

				// Backchannel (producer sendonly): if the producer also advertises
				// the recvonly codec it is sending us (the "apples" it deals in) on
				// its sendonly/backchannel list, that codec is a hard pick for the
				// backchannel — don't let the matcher choose another codec at will.
				// Only fall back to the default match when the recvonly codec isn't
				// on the backchannel list (or the consumer can't send it).
				if prodMedia.Direction == core.DirectionSendonly {
					if pc, cc := matchPreferredBackchannel(prod, prodMedia, consMedia); pc != nil {
						prodCodec, consCodec = pc, cc
					}
				}

				var track *core.Receiver

				switch prodMedia.Direction {
				case core.DirectionRecvonly:
					log.Trace().Msgf("[streams] match cons=%d <= prod=%d", consN, prodN)

					// Step 4. Get recvonly track from producer
					if track, err = prod.GetTrack(prodMedia, prodCodec); err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						prodErrors[prodN] = err
						continue
					}
					// Step 5. Add track to consumer
					if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						continue
					}

				case core.DirectionSendonly:
					log.Trace().Msgf("[streams] match cons=%d => prod=%d", consN, prodN)

					// Step 4. Get recvonly track from consumer (backchannel)
					if track, err = cons.(core.Producer).GetTrack(consMedia, consCodec); err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						continue
					}
					// Step 5. Add track to producer
					if err = prod.AddTrack(prodMedia, prodCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						prodErrors[prodN] = err
						continue
					}
				}

				prodStarts = append(prodStarts, prod)

				if !consMedia.MatchAll() {
					break producers
				}
			}
		}
	}

	// stop producers if they don't have readers
	if s.pending.Add(-1) == 0 {
		s.stopProducers()
	}

	if len(prodStarts) == 0 {
		return formatError(consMedias, prodMedias, prodErrors)
	}

	s.mu.Lock()
	s.consumers = append(s.consumers, cons)
	s.mu.Unlock()

	// there may be duplicates, but that's not a problem
	for _, prod := range prodStarts {
		prod.start()
	}

	return nil
}

func formatError(consMedias, prodMedias []*core.Media, prodErrors []error) error {
	// 1. Return errors if any not nil
	var text string

	for _, err := range prodErrors {
		if err != nil {
			text = appendString(text, err.Error())
		}
	}

	if len(text) != 0 {
		return errors.New("streams: " + text)
	}

	// 2. Return "codecs not matched"
	if prodMedias != nil {
		var prod, cons string

		for _, media := range prodMedias {
			if media.Direction == core.DirectionRecvonly {
				for _, codec := range media.Codecs {
					prod = appendString(prod, media.Kind+":"+codec.PrintName())
				}
			}
		}

		for _, media := range consMedias {
			if media.Direction == core.DirectionSendonly {
				for _, codec := range media.Codecs {
					cons = appendString(cons, media.Kind+":"+codec.PrintName())
				}
			}
		}

		return errors.New("streams: codecs not matched: " + prod + " => " + cons)
	}

	// 3. Return unknown error
	return errors.New("streams: unknown error")
}

func appendString(s, elem string) string {
	if strings.Contains(s, elem) {
		return s
	}
	if len(s) == 0 {
		return elem
	}
	return s + ", " + elem
}

// matchPreferredBackchannel returns the codec pair (producer backchannel codec,
// consumer backchannel codec) for a sendonly/backchannel producer media, biased
// toward the codec the producer is already sending us on its recvonly side.
//
// Rule: if a codec on the producer's recvonly medias (what it sends us, "apples")
// also appears on its sendonly/backchannel list and is supported by the consumer's
// recvonly medias, that codec is the hard choice for the backchannel — never an
// arbitrary first match. Returns nil when no such shared codec exists so the
// caller keeps its default MatchMedia result (nothing is forced).
func matchPreferredBackchannel(prod *Producer, sendonly, consMedia *core.Media) (*core.Codec, *core.Codec) {
	for _, media := range prod.GetMedias() {
		if media.Kind != consMedia.Kind || media.Direction != core.DirectionRecvonly {
			continue
		}
		for _, recvCodec := range media.Codecs {
			// the codec we receive must also be acceptable to send back on the
			// backchannel list and supported by the consumer (caller)
			pc := sendonly.MatchCodec(recvCodec)
			cc := consMedia.MatchCodec(recvCodec)
			if pc != nil && cc != nil {
				return pc, cc
			}
		}
	}
	return nil, nil
}
