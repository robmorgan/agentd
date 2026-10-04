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
//     snapshot, PTY output, input, resize. It has no Hello; AttachSession
//     carries the stream's own feature list instead (CapAttachFeatures).
//   - artifact: GetArtifact or GetHistory. One large transfer, kept off the
//     control stream so it never delays small requests behind it, and off
//     attachments so it never delays terminal traffic (artifact.go).
//   - events: SubscribeEvents. A long-lived, server-to-client stream of
//     session events (lifecycle and attention), in id order, until the
//     client closes it. Events are persisted, so each subscriber is a
//     cursor over state.db: the daemon holds at most one batch of events
//     for it and never buffers for a slow one (see internal/daemon/events.go).
//
// Over QUIC every stream has its own flow control, so a stalled attachment
// or a large transfer never blocks another stream on the same connection.
type StreamRole int

const (
	RoleRequest StreamRole = iota
	RoleControl
	RoleAttach
	RoleArtifact
	RoleEvents
)

func (r StreamRole) String() string {
	switch r {
	case RoleControl:
		return "control"
	case RoleAttach:
		return "attach"
	case RoleArtifact:
		return "artifact"
	case RoleEvents:
		return "events"
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
	case r.SubscribeEvents != nil:
		return RoleEvents
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
	// DaemonID is the daemon's stable id (CapDaemonID), or "".
	DaemonID string
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
	// CapSessionUID: every session incarnation has an immutable random
	// UID, appended to session records and CreateSession results. On an
	// attach stream (see CapAttachFeatures) it adds AttachSession.ExpectUID
	// and Attached.SessionUID.
	CapSessionUID = "session-uid"
	// CapRequestTokens: CreateSession, KillSession, AddWorkspace and
	// RemoveWorkspace carry a client-chosen token (appended), and the
	// daemon answers a request whose token it has seen with the original
	// result instead of acting again. A client that lost its connection
	// after sending one may therefore send it again.
	CapRequestTokens = "request-tokens"
	// CapAttachFeatures: AttachSession may end with the attach stream's
	// own feature list, and Attached then echoes the features the session
	// worker agreed to. Attach streams have no Hello, so this is their
	// handshake. The features below are only meaningful in that list.
	CapAttachFeatures = "attach-features"
	// CapAttachReplace (attach stream): AttachSession.Replaces names an
	// attachment of the same session incarnation that this one replaces,
	// typically the client's own attachment on a connection it lost; the
	// worker drops it at once instead of waiting to notice it is dead.
	CapAttachReplace = "attach-replace"
	// CapAttachResync (attach stream): the worker may send AttachResync, a
	// fresh snapshot that replaces the screen, when it had to drop output
	// for this client because it fell behind.
	CapAttachResync = "attach-resync"
	// CapEvents: the daemon records session events and serves them on
	// events streams (SubscribeEvents) and to ListEvents on the control
	// stream.
	CapEvents = "events"
	// CapSessionActivity: session records carry the activity fields
	// appended to them (session.Record.Activity onwards).
	CapSessionActivity = "session-activity"
	// CapDaemonID: Welcome ends with the daemon's id, the fingerprint of
	// its key (remote/daemon.key), which is stable for the life of its
	// runtime root. With a session's UID it forms the session's global
	// identifier (GlobalSessionID). Sent only to a client whose Hello
	// listed it.
	CapDaemonID = "daemon-id"
	// CapSessionRestart (attach stream): the stream may end with
	// SessionRestarting when the session's worker restarts (a live handoff
	// to a new agentd binary); the client then attaches again.
	CapSessionRestart = "session-restart"
	// CapWorkerHandoff: a session worker accepts HandoffSession. Workers
	// list it in their answer to Hello, so agentd can tell a worker that
	// supports handoff from one started by an older binary.
	CapWorkerHandoff = "worker-handoff"
	// CapTerminalMemory: SessionStats ends with the shadow terminal's
	// memory (TerminalMemory).
	CapTerminalMemory = "terminal-memory"
)

// GlobalSessionID identifies one incarnation of a session across every
// host: the daemon's id and the session's UID. Unlike `host/name` it
// survives renaming the host in hosts.toml and never refers to a newer
// session that reused the name.
func GlobalSessionID(daemonID, uid string) string {
	if daemonID == "" || uid == "" {
		return ""
	}
	return daemonID + "/" + uid
}

// MinProtocolVersion is the oldest protocol version this build speaks.
// Versions in [MinProtocolVersion, ProtocolVersion] can be negotiated.
const MinProtocolVersion uint16 = 1

// Capabilities is what this build supports.
func Capabilities() []string {
	return []string{CapControlStream, CapGitState, CapArtifacts, CapSessionUID, CapRequestTokens, CapAttachFeatures, CapAttachReplace, CapAttachResync, CapRuntimeStats,
		CapEvents, CapSessionActivity, CapDaemonID, CapSessionRestart, CapWorkerHandoff, CapTerminalMemory}
}

// AttachCapabilities are the capabilities that apply to an attach stream,
// listed in AttachSession.Features.
func AttachCapabilities() []string {
	return []string{CapSessionUID, CapAttachReplace, CapAttachResync, CapSessionRestart}
}

// AttachFeatures is the attach-stream features a client may ask for given
// the features it negotiated on its control stream: none unless the daemon
// takes an attach feature list at all.
func AttachFeatures(control Features) []string {
	if !control.Has(CapAttachFeatures) {
		return nil
	}
	var out []string
	for _, c := range AttachCapabilities() {
		if control.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

// IntersectCapabilities is the capabilities in a that b lists too, in a's
// order.
func IntersectCapabilities(a, b []string) []string {
	var out []string
	for _, c := range a {
		if slices.Contains(b, c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
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

// OwnFeatures is every feature this build supports. A session worker
// answers its daemon with them: the daemon is never older than its
// workers (an upgrade replaces the daemon first), and decodes appended
// fields only when they are present.
func OwnFeatures() Features { return NegotiateFeatures(Capabilities(), Capabilities()) }

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

// SessionReplacedMessage is the error for an attach whose ExpectUID no
// longer matches: the session the client knew was removed and another one
// started under its name.
func SessionReplacedMessage(id string) string {
	return fmt.Sprintf("session `%s` is not the session you were attached to: it was removed and a new session was started under the same name; attach to it again if you want it", id)
}

// MaxRequestTokenLen bounds a request token; a token is meant to be a
// random id, not data.
const MaxRequestTokenLen = 64

// TokenSlot returns where req keeps its request token (CapRequestTokens),
// or nil for a request that takes none. Only requests with side effects
// that a retry must not repeat take one: creating, stopping and removing
// sessions, and adding and removing workspaces.
func (r *Request) TokenSlot() *string {
	switch {
	case r.CreateSession != nil:
		return &r.CreateSession.Token
	case r.KillSession != nil:
		return &r.KillSession.Token
	case r.AddWorkspace != nil:
		return &r.AddWorkspace.Token
	case r.RemoveWorkspace != nil:
		return &r.RemoveWorkspace.Token
	}
	return nil
}

// Digest identifies what req asks for, without its token: two requests
// with the same token must have the same digest, or the second is not a
// retry of the first.
func (r *Request) Digest() (string, error) {
	k, payload, err := encodeRequest(r, Features{})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%x", k, payload), nil
}
