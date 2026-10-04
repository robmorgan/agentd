package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// The daemon management protocol is a separate, deliberately tiny protocol
// framed like the main one but at DaemonManagementVersion, with JSON payloads.
// It exists so `agent daemon info|restart|upgrade` keep working when the CLI
// and daemon speak different versions of the main protocol.
const (
	kDaemonStatusRequest    uint16 = 20_001
	kDaemonShutdownRequest  uint16 = 20_002
	kDaemonStatusResponse   uint16 = 21_001
	kDaemonShutdownResponse uint16 = 21_002
	kDaemonErrorResponse    uint16 = 21_099
)

// ManagementRequest is a union; exactly one field is set.
type ManagementRequest struct {
	Status   *struct{}
	Shutdown *ManagementShutdown
}

type ManagementShutdown struct {
	Force bool `json:"force"`
}

// ManagementResponse is a union; exactly one field is set.
type ManagementResponse struct {
	Status   *ManagementStatus
	Shutdown *ManagementShutdownResult
	Error    *ErrorResponse
}

type ManagementStatus struct {
	DaemonVersion   string `json:"daemon_version"`
	ProtocolVersion uint16 `json:"protocol_version"`
	PID             uint32 `json:"pid"`
	Root            string `json:"root"`
	Socket          string `json:"socket"`
	RunningSessions bool   `json:"running_sessions"`
	// Remote is where the QUIC listener is bound; empty when remote access
	// is off or not listening yet, in which case RemoteError says why.
	Remote      string `json:"remote,omitempty"`
	RemoteError string `json:"remote_error,omitempty"`
	// OpenStreams counts the streams the daemon is serving over every
	// transport: requests, control streams and attachments.
	OpenStreams int `json:"open_streams"`
	// Connections lists the open remote (QUIC) connections.
	Connections []ManagementConnection `json:"connections,omitempty"`
}

// ManagementConnection describes one open remote connection.
type ManagementConnection struct {
	Fingerprint string `json:"fingerprint"`
	// Name is the name the key was authorized under, if any.
	Name          string    `json:"name,omitempty"`
	Remote        string    `json:"remote"`
	ConnectedAt   time.Time `json:"connected_at"`
	StreamsOpened uint64    `json:"streams_opened"`
	StreamsOpen   int64     `json:"streams_open"`
	// RTTMicros is QUIC's smoothed round-trip time, in microseconds.
	RTTMicros     int64  `json:"rtt_us"`
	BytesSent     uint64 `json:"bytes_sent"`
	BytesReceived uint64 `json:"bytes_received"`
	PacketsSent   uint64 `json:"packets_sent"`
	PacketsLost   uint64 `json:"packets_lost"`
}

type ManagementShutdownResult struct {
	Stopped         bool   `json:"stopped"`
	RunningSessions bool   `json:"running_sessions"`
	Message         string `json:"message"`
}

// DecodeError reports a well-framed message at a supported version that
// could not be decoded (an unknown kind, a bad payload). The peer
// is still speaking our framing, so it can be sent an error reply.
type DecodeError struct {
	Version uint16
	Err     error
}

func (e *DecodeError) Error() string { return e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// ReadIncoming reads the first frame of a daemon connection, which may belong
// to either protocol. It returns (nil, nil, nil) on a clean EOF, a
// *VersionError for a main-protocol frame at an unsupported version, and a
// *DecodeError for a frame that is framed correctly but cannot be decoded.
func ReadIncoming(r io.Reader) (*Request, *ManagementRequest, error) {
	h, payload, err := readRawFrame(r)
	if err != nil || h == nil {
		return nil, nil, err
	}
	if h.version == DaemonManagementVersion {
		m, err := decodeManagementRequest(h.kind, payload)
		if err != nil {
			return nil, nil, &DecodeError{Version: h.version, Err: err}
		}
		return nil, m, nil
	}
	if !supportedVersion(h.version) {
		return nil, nil, &VersionError{Version: h.version}
	}
	if h.tagged {
		return nil, nil, &DecodeError{Version: h.version, Err: errUnexpectedTag}
	}
	req, err := decodeRequest(kind(h.kind), payload, Features{})
	if err != nil {
		return nil, nil, &DecodeError{Version: h.version, Err: err}
	}
	return req, nil, nil
}

func decodeManagementRequest(k uint16, payload []byte) (*ManagementRequest, error) {
	switch k {
	case kDaemonStatusRequest:
		if len(payload) != 0 {
			return nil, fmt.Errorf("unexpected status request payload")
		}
		return &ManagementRequest{Status: Empty}, nil
	case kDaemonShutdownRequest:
		var s ManagementShutdown
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil, err
		}
		return &ManagementRequest{Shutdown: &s}, nil
	}
	return nil, fmt.Errorf("unknown daemon management request kind `%d`", k)
}

func WriteManagementRequest(w io.Writer, req *ManagementRequest) error {
	switch {
	case req.Status != nil:
		return writeFrame(w, DaemonManagementVersion, kDaemonStatusRequest, nil)
	case req.Shutdown != nil:
		payload, err := json.Marshal(req.Shutdown)
		if err != nil {
			return err
		}
		return writeFrame(w, DaemonManagementVersion, kDaemonShutdownRequest, payload)
	}
	return fmt.Errorf("empty management request")
}

func WriteManagementResponse(w io.Writer, resp *ManagementResponse) error {
	var (
		k       uint16
		payload []byte
		err     error
	)
	switch {
	case resp.Status != nil:
		k = kDaemonStatusResponse
		payload, err = json.Marshal(resp.Status)
	case resp.Shutdown != nil:
		k = kDaemonShutdownResponse
		payload, err = json.Marshal(resp.Shutdown)
	case resp.Error != nil:
		k = kDaemonErrorResponse
		payload, err = json.Marshal(map[string]string{"message": resp.Error.Message})
	default:
		return fmt.Errorf("empty management response")
	}
	if err != nil {
		return err
	}
	return writeFrame(w, DaemonManagementVersion, k, payload)
}

// ReadManagementResponse returns (nil, nil) on a clean EOF.
func ReadManagementResponse(r io.Reader) (*ManagementResponse, error) {
	h, payload, err := readRawFrame(r)
	if err != nil || h == nil {
		return nil, err
	}
	if h.version != DaemonManagementVersion {
		return nil, fmt.Errorf("unsupported daemon management protocol version `%d`", h.version)
	}
	switch h.kind {
	case kDaemonStatusResponse:
		var s ManagementStatus
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil, err
		}
		return &ManagementResponse{Status: &s}, nil
	case kDaemonShutdownResponse:
		var s ManagementShutdownResult
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil, err
		}
		return &ManagementResponse{Shutdown: &s}, nil
	case kDaemonErrorResponse:
		var e struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload, &e); err != nil {
			return nil, err
		}
		return &ManagementResponse{Error: &ErrorResponse{Message: e.Message}}, nil
	}
	return nil, fmt.Errorf("unknown daemon management response kind `%d`", h.kind)
}
