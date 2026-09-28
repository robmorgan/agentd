package protocol

import (
	"encoding/json"
	"fmt"
	"io"
)

// The daemon management protocol is a separate, deliberately tiny protocol
// framed like the main one but at DaemonManagementVersion, with JSON payloads.
// It exists so `agent daemon status|stop` keep working across main protocol
// bumps. It mirrors the DaemonManagement* types in
// crates/agentd-shared/src/protocol.rs.
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
}

type ManagementShutdownResult struct {
	Stopped         bool   `json:"stopped"`
	RunningSessions bool   `json:"running_sessions"`
	Message         string `json:"message"`
}

// DecodeError reports a well-framed message at a supported version that
// could not be decoded (an unknown or retired kind, a bad payload). The peer
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
	if h.version != ProtocolVersion {
		return nil, nil, &VersionError{Version: h.version}
	}
	req, err := decodeRequest(kind(h.kind), payload)
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
