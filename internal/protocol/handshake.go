package protocol

import (
	"fmt"
	"io"
	"slices"
)

// Streams and their roles.
//
// The protocol's unit is a stream: one Unix socket connection, or one QUIC
// stream on a long-lived QUIC connection. The first frame a client sends on a
// stream decides the stream's role for its whole life, so no separate
// negotiation round trip is needed (Request.Role):
//
//   - request: any other request. One response (or an error) follows and
//     the stream ends.
//   - control: Hello. The daemon answers Welcome, and the stream then carries
//     any number of one-shot requests, each tagged with a request id, whose
//     tagged responses may arrive in any order. It lives as long as the
//     client wants it.
//   - attach: AttachSession. The interactive stream of one attachment:
//     snapshot, PTY output, input, resize.
//   - artifact: GetArtifact or GetHistory. One large transfer, kept off the
//     control stream so it never delays small requests behind it, and off
//     attachments so it never delays terminal traffic (artifact.go).
//
// Over QUIC every stream has its own flow control, so a stalled attachment
// or a large transfer never blocks another stream on the same connection.
type StreamRole int

const (
	RoleRequest StreamRole = iota
	RoleControl
	RoleAttach
	RoleArtifact
)

func (r StreamRole) String() string {
	switch r {
	case RoleControl:
		return "control"
	case RoleAttach:
		return "attach"
	case RoleArtifact:
		return "artifact"
	}
	return "request"
}

// Role is the role of a stream that starts with r.
func (r *Request) Role() StreamRole {
	switch {
	case r.Hello != nil:
		return RoleControl
	case r.AttachSession != nil:
		return RoleAttach
	case r.GetHistory != nil, r.GetArtifact != nil:
		return RoleArtifact
	}
	return RoleRequest
}

// Hello opens a control stream. It is the connection handshake: the client
// says which protocol versions and optional features it supports, and the
// daemon answers with a Welcome naming the version both will speak and the
// features it supports, so neither side ever sends the other something it
// cannot decode.
//
// Hello is always framed at the client's MinVersion, which a daemon that
// supports any version in the range can read. A daemon that cannot answers
// with an Error frame in the client's framing, as for any other version
// mismatch.
type Hello struct {
	MinVersion, MaxVersion uint16
	// Client names the client and its version, for logs.
	Client       string
	Capabilities []string
}

// Welcome answers Hello.
type Welcome struct {
	// Version is the protocol version both sides speak from here on.
	Version       uint16
	DaemonVersion string
	// Capabilities are the optional features this daemon supports. A
	// client uses a feature only if it is listed here.
	Capabilities []string
	Host         HostInfo
}

// HostInfo describes the daemon's machine, so a client can tell machines
// apart and judge where to run work.
type HostInfo struct {
	Name        string
	OS          string
	Arch        string
	CPUs        uint32
	MemoryBytes uint64
	// Agents are the agent names configured on this host, in config order.
	Agents       []string
	DefaultAgent string
}

// Capabilities. Each names an optional feature both sides must support
// before either uses it. Additions to the protocol that an older peer could
// not decode (a new message kind it would receive, a field appended to an
// existing message) are introduced together with a capability.
const (
	// CapControlStream: the daemon serves tagged requests on the stream
	// that started with Hello.
	CapControlStream = "control-stream"
	// CapGitState: GetGitState and its GitState response.
	CapGitState = "git-state"
	// CapArtifacts: ListArtifacts, its Artifacts response, and artifact
	// streams (GetArtifact, answered with ArtifactChunk frames).
	CapArtifacts = "artifacts"
	// CapRuntimeStats: GetSessionStats and GetDaemonStats (stats.go).
	CapRuntimeStats = "runtime-stats"
)

// MinProtocolVersion is the oldest protocol version this build speaks.
// Versions in [MinProtocolVersion, ProtocolVersion] can be negotiated.
const MinProtocolVersion uint16 = 1

// Capabilities is what this build supports.
func Capabilities() []string {
	return []string{CapControlStream, CapGitState, CapArtifacts, CapRuntimeStats}
}

// HasCapability reports whether caps lists c.
func HasCapability(caps []string, c string) bool {
	return slices.Contains(caps, c)
}

// NegotiateVersion picks the newest version in both this build's range and
// the client's, or explains why there is none.
func NegotiateVersion(clientMin, clientMax uint16) (uint16, error) {
	if clientMin > clientMax {
		return 0, fmt.Errorf("client offered no protocol versions (min %d > max %d)", clientMin, clientMax)
	}
	v := min(clientMax, ProtocolVersion)
	if v < max(clientMin, MinProtocolVersion) {
		return 0, fmt.Errorf("agentd speaks protocol versions %d-%d but the client needs %d-%d; upgrade the agent CLI or the daemon",
			MinProtocolVersion, ProtocolVersion, clientMin, clientMax)
	}
	return v, nil
}

// NewHello is this build's Hello.
func NewHello(client string) *Hello {
	return &Hello{MinVersion: MinProtocolVersion, MaxVersion: ProtocolVersion, Client: client, Capabilities: Capabilities()}
}

// Features is the set of optional features both peers of a stream
// support: the capabilities both listed in the handshake. It decides which
// optional fields encoders write and decoders expect; the zero value is the
// base encoding.
//
// A capability that adds a field to an existing message (or to a struct
// such as a session record, even one inside a list) appends it to the end
// of that message or struct, and encoders and decoders handle it only when
// Features has the capability.
type Features struct{ set map[string]bool }

// NegotiateFeatures is the features in both ours and theirs.
func NegotiateFeatures(ours, theirs []string) Features {
	f := Features{set: map[string]bool{}}
	for _, c := range theirs {
		if slices.Contains(ours, c) {
			f.set[c] = true
		}
	}
	return f
}

// Has reports whether both peers support capability c.
func (f Features) Has(c string) bool { return f.set[c] }

// List returns the features, sorted.
func (f Features) List() []string {
	out := make([]string, 0, len(f.set))
	for c := range f.set {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// WriteTaggedRequest writes req on a control stream, tagged with id, with
// the stream's negotiated features.
func WriteTaggedRequest(w io.Writer, f Features, id uint32, req *Request) error {
	k, payload, err := encodeRequest(req, f)
	if err != nil {
		return err
	}
	return writeFrameTagged(w, ProtocolVersion, uint16(k), true, id, payload)
}

// WriteTaggedResponse writes resp on a control stream, tagged with the id of
// the request it answers.
func WriteTaggedResponse(w io.Writer, f Features, id uint32, resp *Response) error {
	k, payload, err := encodeResponse(resp, f)
	if err != nil {
		return err
	}
	return writeFrameTagged(w, ProtocolVersion, uint16(k), true, id, payload)
}

// ReadTaggedRequest reads the next request on a control stream. It returns
// a nil request and nil error on a clean EOF. A request that is framed
// correctly but cannot be decoded returns a *DecodeError together with its
// id, and leaves the stream at the next frame, so the daemon can answer it
// and carry on.
func ReadTaggedRequest(r io.Reader, f Features) (req *Request, id uint32, tagged bool, err error) {
	h, payload, err := readStandardFrame(r)
	if err != nil || h == nil {
		return nil, 0, false, err
	}
	req, err = decodeRequest(kind(h.kind), payload, f)
	if err != nil {
		return nil, h.id, h.tagged, &DecodeError{Version: h.version, Err: err}
	}
	return req, h.id, h.tagged, nil
}

// ReadTaggedResponse reads the next response on a control stream. It returns
// a nil response and nil error on a clean EOF.
func ReadTaggedResponse(r io.Reader, f Features) (resp *Response, id uint32, tagged bool, err error) {
	h, payload, err := readStandardFrame(r)
	if err != nil || h == nil {
		return nil, 0, false, err
	}
	resp, err = decodeResponse(kind(h.kind), payload, f)
	if err != nil {
		return nil, h.id, h.tagged, err
	}
	return resp, h.id, h.tagged, nil
}
