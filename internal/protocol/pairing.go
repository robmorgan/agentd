package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The pairing protocol runs on the one stream of a pairing connection: a
// QUIC connection whose TLS handshake negotiated the pairing ALPN
// (transport.PairALPN) rather than the agent protocol's. Its client is not
// authorized yet, so the stream carries nothing but this exchange, which
// proves that both ends know the same short pairing code (a CPace PAKE; see
// internal/transport/pair.go) and then authorizes the client and pins the
// daemon:
//
//	client → daemon  PairHello     the client's name and CPace message
//	daemon → client  PairChallenge the daemon's CPace message
//	client → daemon  PairProof     the client's key confirmation
//	daemon → client  PairAccepted  the daemon's key confirmation
//
// Either side may instead send PairError and end the exchange.
//
// Like the management protocol, pairing is framed at DaemonManagementVersion
// with JSON payloads, so a CLI can pair with a daemon whatever versions of
// the main protocol the two speak.
const (
	kPairHello     uint16 = 30_001
	kPairChallenge uint16 = 30_002
	kPairProof     uint16 = 30_003
	kPairAccepted  uint16 = 30_004
	kPairError     uint16 = 30_099
)

// MaxPairFramePayload bounds a pairing frame. The peer is not authenticated
// yet, so it may not make this end allocate more than a few kilobytes.
const MaxPairFramePayload = 4 << 10

// PairMessage is a union; exactly one field is set.
type PairMessage struct {
	Hello     *PairHello
	Challenge *PairChallenge
	Proof     *PairProof
	Accepted  *PairAccepted
	Error     *ErrorResponse
}

type PairHello struct {
	// Name is the client machine's name, recorded with its key on the
	// daemon (as `agtd remote authorize` would).
	Name string `json:"name"`
	// Message is the client's CPace message.
	Message []byte `json:"message"`
}

type PairChallenge struct {
	// Message is the daemon's CPace message.
	Message []byte `json:"message"`
	// Host is the daemon machine's name, for messages.
	Host string `json:"host"`
}

type PairProof struct {
	MAC []byte `json:"mac"`
}

type PairAccepted struct {
	MAC []byte `json:"mac"`
}

func WritePairMessage(w io.Writer, m *PairMessage) error {
	var (
		k uint16
		v any
	)
	switch {
	case m.Hello != nil:
		k, v = kPairHello, m.Hello
	case m.Challenge != nil:
		k, v = kPairChallenge, m.Challenge
	case m.Proof != nil:
		k, v = kPairProof, m.Proof
	case m.Accepted != nil:
		k, v = kPairAccepted, m.Accepted
	case m.Error != nil:
		k, v = kPairError, map[string]string{"message": m.Error.Message}
	default:
		return errors.New("empty pairing message")
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > MaxPairFramePayload {
		return fmt.Errorf("pairing message too large: %d bytes", len(payload))
	}
	return writeFrame(w, DaemonManagementVersion, k, payload)
}

// ReadPairMessage reads one pairing message. A clean EOF before it is
// io.ErrUnexpectedEOF: the exchange always expects another message.
func ReadPairMessage(r io.Reader) (*PairMessage, error) {
	h, payload, err := readRawFrameLimit(r, MaxPairFramePayload)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, io.ErrUnexpectedEOF
	}
	if h.version != DaemonManagementVersion || h.tagged {
		return nil, fmt.Errorf("not a pairing frame (version %d)", h.version)
	}
	var m PairMessage
	switch h.kind {
	case kPairHello:
		m.Hello = &PairHello{}
		err = json.Unmarshal(payload, m.Hello)
	case kPairChallenge:
		m.Challenge = &PairChallenge{}
		err = json.Unmarshal(payload, m.Challenge)
	case kPairProof:
		m.Proof = &PairProof{}
		err = json.Unmarshal(payload, m.Proof)
	case kPairAccepted:
		m.Accepted = &PairAccepted{}
		err = json.Unmarshal(payload, m.Accepted)
	case kPairError:
		var e struct {
			Message string `json:"message"`
		}
		err = json.Unmarshal(payload, &e)
		m.Error = &ErrorResponse{Message: e.Message}
	default:
		return nil, fmt.Errorf("unknown pairing message kind `%d`", h.kind)
	}
	if err != nil {
		return nil, fmt.Errorf("bad pairing message: %w", err)
	}
	return &m, nil
}
