// Package protocol implements the framed binary wire protocol shared by the
// agent CLI, the daemon, and session workers. Golden-frame tests pin its
// bytes, so a CLI and a daemon of different builds (or on different
// machines) keep understanding each other.
package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/robmorgan/agentd/internal/session"
)

const (
	ProtocolVersion uint16 = 1
	// DaemonManagementVersion frames the daemon management protocol
	// (management.go). It is not a version of the main protocol and never
	// changes, so status and shutdown keep working across protocol versions.
	DaemonManagementVersion uint16 = 0

	frameMagic     uint32 = 0x4147_4450
	frameHeaderLen        = 16

	// MaxFramePayload bounds a frame's declared payload length, which comes
	// straight off the wire, so one bad header cannot make a peer allocate
	// gigabytes. It comfortably fits a snapshot or history of the maximum
	// retained scrollback.
	MaxFramePayload = 64 << 20
)

// Request is the union of all client to daemon and daemon to worker requests.
// Exactly one field is set. The kind is encoded by which pointer is non-nil.
type Request struct {
	GetDaemonInfo    *struct{}
	ShutdownDaemon   *struct{}
	CreateSession    *CreateSession
	KillSession      *KillSession
	AttachSession    *AttachSession
	AttachResize     *Geometry
	DetachSession    *DetachSession
	DetachAttachment *DetachAttachment
	AttachInput      *Bytes
	AttachSnapshot   *struct{}
	SendInput        *SendInput
	GetSession       *SessionRef
	ListSessions     *struct{}
	ListAttachments  *SessionRef
	GetHistory       *GetHistory
	ListWorkspaces   *struct{}
	AddWorkspace     *AddWorkspace
	RemoveWorkspace  *WorkspaceRef
	// Hello opens a control stream; see handshake.go.
	Hello *Hello
	// Git state and artifacts (CapGitState, CapArtifacts); see artifact.go.
	GetGitState   *SessionRef
	ListArtifacts *SessionRef
	GetArtifact   *GetArtifact
	// Resource usage; see stats.go.
	GetSessionStats *GetSessionStats
	GetDaemonStats  *struct{}
	// SubscribeEvents opens an events stream (CapEvents).
	SubscribeEvents *SubscribeEvents
	// ListEvents is a one-shot query of recorded events (CapEvents).
	ListEvents *ListEvents
	// HandoffSession asks a session worker to replace its executable
	// without stopping the agent (CapWorkerHandoff); see handoff.go.
	HandoffSession *HandoffSession
}

type SessionRef struct{ SessionID string }
type Bytes struct{ Data []byte }

type CreateSession struct {
	// Cwd is the directory the agent runs in, resolved on the daemon's
	// machine. Without a Workspace it must be absolute or start with `~/`.
	// With one, it is empty (the workspace root) or relative to the root.
	// The daemon checks that it exists and nothing more.
	Cwd   string
	Name  *string
	Agent string
	Model *string
	// Workspace names a directory in the daemon's config (`[workspaces]`).
	Workspace *string
	// Token (CapRequestTokens) identifies this request across retries; see
	// RequestToken. Empty means none.
	Token string
}

// AddWorkspace registers Path under Name. Path is resolved on the daemon's
// machine: it must be absolute or start with `~/`, and exist.
type AddWorkspace struct {
	Name, Path string
	// Token (CapRequestTokens); see CreateSession.Token.
	Token string
}

// WorkspaceRef names a workspace to remove.
type WorkspaceRef struct {
	Name string
	// Token (CapRequestTokens); see CreateSession.Token.
	Token string
}

type KillSession struct {
	SessionID string
	Remove    bool
	// Token (CapRequestTokens); see CreateSession.Token.
	Token string
}

type Geometry struct {
	Cols, Rows, PixelWidth, PixelHeight uint16
}

type AttachSession struct {
	SessionID string
	Kind      session.AttachmentKind
	Geometry
	// Features (CapAttachFeatures) are the attach-stream capabilities the
	// client wants on this stream. Attach streams have no handshake, so a
	// client sends them only if the daemon's Welcome listed them; they are
	// encoded after Geometry when non-empty, followed by the fields of the
	// features listed, in the order below.
	Features []string
	// ExpectUID (CapSessionUID) refuses the attach unless the session's
	// current incarnation has this UID: a reattach must never land on a
	// new session that reused the name. Empty means any incarnation.
	ExpectUID string
	// Replaces (CapAttachReplace) is the id of an attachment this one
	// replaces (the client's own, on a connection it lost). It is honoured
	// only together with a matching ExpectUID, since attach ids are only
	// unique within one incarnation.
	Replaces string
}

// HasFeature reports whether the attach stream asked for capability c.
func (a *AttachSession) HasFeature(c string) bool { return slices.Contains(a.Features, c) }

type DetachSession struct {
	SessionID string
	All       bool
}

type DetachAttachment struct {
	SessionID, AttachID string
}

type SendInput struct {
	SessionID       string
	Data            []byte
	SourceSessionID *string
}

type GetHistory struct {
	SessionID string
	VT        bool
}

// SubscribeEvents opens an events stream: the daemon sends recorded events
// (see session.Event) as Event frames, first the backlog and then live, in
// id order, until the client closes the stream.
type SubscribeEvents struct {
	// AfterID resumes after the event with this id: every retained event
	// with a higher id is sent. Without it the stream starts with the
	// newest Tail events.
	AfterID *uint64
	// SessionID limits the stream to one session's events.
	SessionID *string
	// Tail is how many past events to send first when AfterID is unset
	// (0: only new ones).
	Tail uint32
}

// ListEvents asks for recorded events, answered with Events, oldest first.
type ListEvents struct {
	// AfterID pages forward: the oldest Limit events after it. Without
	// it, the newest Limit events.
	AfterID   *uint64
	SessionID *string
	// Limit caps the answer (0: the daemon's default).
	Limit uint32
}

// Response is the union of all responses. Exactly one field is set.
type Response struct {
	DaemonInfo     *DaemonInfo
	CreateSession  *session.CreateResult
	KillSession    *KillSessionResult
	Attached       *Attached
	AttachSnapshot *Bytes
	SessionEnded   *SessionEnded
	InputAccepted  *struct{}
	Session        *session.Record
	Sessions       *[]session.Record
	Attachments    *[]session.AttachmentRecord
	History        *History
	PtyOutput      *Bytes
	EndOfStream    *struct{}
	Error          *ErrorResponse
	Ok             *struct{}
	Workspaces     *[]session.Workspace
	Workspace      *session.Workspace
	Welcome        *Welcome
	GitState       *session.GitState
	Artifacts      *[]session.Artifact
	ArtifactChunk  *Bytes
	SessionStats   *SessionStats
	DaemonStats    *DaemonStats
	// AttachResync (CapAttachResync) is an unsolicited snapshot on an
	// attach stream: the worker dropped output this client fell behind
	// on, and this replaces the screen. Like AttachSnapshot it is an exact
	// boundary: the output that follows continues from it.
	AttachResync *Bytes
	Event        *session.Event
	Events       *[]session.Event
	// HandedOff answers HandoffSession (CapWorkerHandoff).
	HandedOff *HandedOff
	// SessionRestarting (CapSessionRestart) ends an attach stream whose
	// session worker is about to restart; see handoff.go.
	SessionRestarting *SessionRestarting
}

type DaemonInfo struct {
	DaemonVersion   string
	ProtocolVersion uint16
}

type KillSessionResult struct{ Removed, WasRunning bool }
type Attached struct {
	AttachID string
	Snapshot []byte
	// Features are the attach-stream capabilities in effect, sent (after
	// Snapshot) only when the AttachSession listed some. Fields of the
	// features listed follow in the order below.
	Features []string
	// SessionUID (CapSessionUID) is the UID of the incarnation attached
	// to, for the client to expect on a reattach.
	SessionUID string
}

// HasFeature reports whether the attach stream uses capability c.
func (a *Attached) HasFeature(c string) bool { return slices.Contains(a.Features, c) }

type History struct{ Data string }
type ErrorResponse struct{ Message string }

type SessionEnded struct {
	SessionID string
	Status    session.Status
	ExitCode  *int32
	Error     *string
}

var Empty = &struct{}{}

func OkResponse() *Response          { return &Response{Ok: Empty} }
func EndOfStreamResponse() *Response { return &Response{EndOfStream: Empty} }
func ErrorResponsef(format string, args ...any) *Response {
	return &Response{Error: &ErrorResponse{Message: fmt.Sprintf(format, args...)}}
}

type kind uint16

// Message kinds. kErrorResponse and its payload (one string) must never
// change: a peer on another protocol version is refused with an Error frame
// in its own framing (WriteErrorAtVersion), which it can only decode if both
// stay fixed.
const (
	kGetDaemonInfoRequest    kind = 1
	kShutdownDaemonRequest   kind = 2
	kCreateSessionRequest    kind = 3
	kKillSessionRequest      kind = 4
	kAttachSessionRequest    kind = 5
	kAttachInputRequest      kind = 6
	kAttachResizeRequest     kind = 7
	kAttachSnapshotRequest   kind = 8
	kDetachSessionRequest    kind = 9
	kDetachAttachmentRequest kind = 10
	kSendInputRequest        kind = 11
	kGetSessionRequest       kind = 12
	kListSessionsRequest     kind = 13
	kListAttachmentsRequest  kind = 14
	kGetHistoryRequest       kind = 15
	kListWorkspacesRequest   kind = 16
	kAddWorkspaceRequest     kind = 17
	kRemoveWorkspaceRequest  kind = 18
	kHelloRequest            kind = 19
	// 20-29: session events (events.go in the daemon).
	kSubscribeEventsRequest kind = 20
	kListEventsRequest      kind = 21
	kGetGitStateRequest     kind = 30
	kListArtifactsRequest   kind = 31
	kGetArtifactRequest     kind = 32
	// 70-79: resource usage (stats.go).
	kGetSessionStatsRequest kind = 70
	kGetDaemonStatsRequest  kind = 71
	// 60-69: worker handoff (handoff.go).
	kHandoffSessionRequest kind = 60

	kDaemonInfoResponse     kind = 101
	kCreateSessionResponse  kind = 102
	kKillSessionResponse    kind = 103
	kAttachedResponse       kind = 104
	kAttachSnapshotResponse kind = 105
	kSessionEndedResponse   kind = 106
	kInputAcceptedResponse  kind = 107
	kSessionResponse        kind = 108
	kSessionsResponse       kind = 109
	kAttachmentsResponse    kind = 110
	kHistoryResponse        kind = 111
	kPtyOutputResponse      kind = 112
	kEndOfStreamResponse    kind = 113
	kErrorResponse          kind = 114
	kOkResponse             kind = 115
	kWorkspacesResponse     kind = 116
	kWorkspaceResponse      kind = 117
	kWelcomeResponse        kind = 118
	// 120-129: session events.
	kEventResponse         kind = 120
	kEventsResponse        kind = 121
	kGitStateResponse      kind = 130
	kArtifactsResponse     kind = 131
	kArtifactChunkResponse kind = 132
	// 170-179: resource usage (stats.go).
	kSessionStatsResponse kind = 170
	kDaemonStatsResponse  kind = 171

	// Session resumption (kinds 150-159).
	kAttachResyncResponse kind = 150

	// Worker handoff (kinds 160-169).
	kHandedOffResponse         kind = 160
	kSessionRestartingResponse kind = 161
)

// ---------------------------------------------------------------------------
// Framing

// Frame flags. A flag changes how the payload is laid out, so a frame with a
// flag this build does not know is refused rather than misread.
const (
	// flagRequestID marks a frame whose payload starts with a u32 request
	// id. Only control streams (handshake.go) carry them.
	flagRequestID uint16 = 1 << 0

	knownFlags = flagRequestID
)

type frameHeader struct {
	version uint16
	kind    uint16
	// tagged reports that the frame carried a request id.
	tagged bool
	id     uint32
}

func writeFrame(w io.Writer, version uint16, k uint16, payload []byte) error {
	return writeFrameTagged(w, version, k, false, 0, payload)
}

func writeFrameTagged(w io.Writer, version uint16, k uint16, tagged bool, id uint32, payload []byte) error {
	var flags uint16
	prefix := 0
	if tagged {
		flags |= flagRequestID
		prefix = 4
	}
	if prefix+len(payload) > MaxFramePayload {
		return fmt.Errorf("frame payload too large: %d bytes (limit %d)", prefix+len(payload), MaxFramePayload)
	}
	var header [frameHeaderLen]byte
	putFrameHeader(header[:], version, k, flags, prefix+len(payload))
	buf := make([]byte, 0, frameHeaderLen+prefix+len(payload))
	buf = append(buf, header[:]...)
	if tagged {
		buf = binary.LittleEndian.AppendUint32(buf, id)
	}
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	if err != nil {
		return err
	}
	if f, ok := w.(*bufio.Writer); ok {
		return f.Flush()
	}
	return nil
}

// putFrameHeader writes a frame header into h, which is frameHeaderLen
// bytes long.
func putFrameHeader(h []byte, version, k, flags uint16, payloadLen int) {
	binary.LittleEndian.PutUint32(h[0:4], frameMagic)
	binary.LittleEndian.PutUint16(h[4:6], version)
	binary.LittleEndian.PutUint16(h[6:8], k)
	binary.LittleEndian.PutUint16(h[8:10], flags)
	binary.LittleEndian.PutUint16(h[10:12], 0)
	binary.LittleEndian.PutUint32(h[12:16], uint32(payloadLen))
}

// readRawFrame returns (nil, nil) on a clean EOF before any header byte.
func readRawFrame(r io.Reader) (*frameHeader, []byte, error) {
	var header [frameHeaderLen]byte
	n, err := r.Read(header[:1])
	if n == 0 {
		if err == nil || errors.Is(err, io.EOF) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if _, err := io.ReadFull(r, header[1:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, nil, errors.New("truncated frame header")
		}
		return nil, nil, err
	}
	if binary.LittleEndian.Uint32(header[0:4]) != frameMagic {
		return nil, nil, errors.New("invalid frame magic")
	}
	h := &frameHeader{
		version: binary.LittleEndian.Uint16(header[4:6]),
		kind:    binary.LittleEndian.Uint16(header[6:8]),
	}
	flags := binary.LittleEndian.Uint16(header[8:10])
	payloadLen := binary.LittleEndian.Uint32(header[12:16])
	if payloadLen > MaxFramePayload {
		return nil, nil, fmt.Errorf("frame payload too large: %d bytes (limit %d)", payloadLen, MaxFramePayload)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, nil, err
	}
	// The payload is consumed before the flags are judged, so a refused
	// frame leaves the stream at the next frame boundary.
	if flags&^knownFlags != 0 {
		return nil, nil, &DecodeError{Version: h.version, Err: fmt.Errorf("unsupported frame flags %#04x", flags)}
	}
	if flags&flagRequestID != 0 {
		if len(payload) < 4 {
			return nil, nil, &DecodeError{Version: h.version, Err: errors.New("truncated request id")}
		}
		h.tagged = true
		h.id = binary.LittleEndian.Uint32(payload)
		payload = payload[4:]
	}
	return h, payload, nil
}

// readStandardFrame returns a nil header on a clean EOF before any frame.
func readStandardFrame(r io.Reader) (*frameHeader, []byte, error) {
	h, payload, err := readRawFrame(r)
	if err != nil || h == nil {
		return nil, nil, err
	}
	if !supportedVersion(h.version) {
		return nil, nil, &VersionError{Version: h.version}
	}
	return h, payload, nil
}

func supportedVersion(v uint16) bool {
	return v >= MinProtocolVersion && v <= ProtocolVersion
}

// errUnexpectedTag reports a request id on a stream that does not use them.
var errUnexpectedTag = errors.New("unexpected request id outside a control stream")

// VersionError reports a frame whose protocol version this build does not
// speak. The frame's payload has been consumed.
type VersionError struct{ Version uint16 }

func (e *VersionError) Error() string {
	return fmt.Sprintf("unsupported protocol version `%d`", e.Version)
}

// WriteErrorAtVersion writes an Error response framed with the given protocol
// version. The Error kind and its payload (one length-prefixed string) have
// not changed across versions, so a peer speaking an older or newer protocol
// can still decode why it was turned away.
func WriteErrorAtVersion(w io.Writer, version uint16, message string) error {
	e := &encoder{}
	e.str(message)
	if e.err != nil {
		return e.err
	}
	return writeFrame(w, version, uint16(kErrorResponse), e.buf)
}

// WriteRequest writes req in the base encoding, which every peer of this
// protocol version decodes. Streams whose peers agreed on optional
// features use WriteRequestWith.
func WriteRequest(w io.Writer, req *Request) error {
	return WriteRequestWith(w, Features{}, req)
}

// WriteRequestWith writes req, including the optional fields of features f.
func WriteRequestWith(w io.Writer, f Features, req *Request) error {
	k, payload, err := encodeRequest(req, f)
	if err != nil {
		return err
	}
	return writeFrame(w, ProtocolVersion, uint16(k), payload)
}

// WriteResponse writes resp in the base encoding.
func WriteResponse(w io.Writer, resp *Response) error {
	return WriteResponseWith(w, Features{}, resp)
}

// WriteResponseWith writes resp, including the optional fields of features f.
func WriteResponseWith(w io.Writer, f Features, resp *Response) error {
	k, payload, err := encodeResponse(resp, f)
	if err != nil {
		return err
	}
	return writeFrame(w, ProtocolVersion, uint16(k), payload)
}

// ReadRequest returns (nil, nil) when the peer closed the connection cleanly.
func ReadRequest(r io.Reader) (*Request, error) {
	return ReadRequestWith(r, Features{})
}

// ReadRequestWith reads a request written with features f.
func ReadRequestWith(r io.Reader, f Features) (*Request, error) {
	h, payload, err := readStandardFrame(r)
	if err != nil || h == nil {
		return nil, err
	}
	if h.tagged {
		return nil, errUnexpectedTag
	}
	return decodeRequest(kind(h.kind), payload, f)
}

// ReadResponse returns (nil, nil) when the peer closed the connection cleanly.
func ReadResponse(r io.Reader) (*Response, error) {
	return ReadResponseWith(r, Features{})
}

// ReadResponseWith reads a response written with features f.
func ReadResponseWith(r io.Reader, f Features) (*Response, error) {
	h, payload, err := readStandardFrame(r)
	if err != nil || h == nil {
		return nil, err
	}
	if h.tagged {
		return nil, errUnexpectedTag
	}
	return decodeResponse(kind(h.kind), payload, f)
}

// ---------------------------------------------------------------------------
// Encoding

func encodeRequest(req *Request, f Features) (kind, []byte, error) {
	e := &encoder{f: f}
	switch {
	case req.GetDaemonInfo != nil:
		return kGetDaemonInfoRequest, nil, nil
	case req.ShutdownDaemon != nil:
		return kShutdownDaemonRequest, nil, nil
	case req.CreateSession != nil:
		c := req.CreateSession
		e.str(c.Cwd)
		e.optStr(c.Name)
		e.str(c.Agent)
		e.optStr(c.Model)
		e.optStr(c.Workspace)
		e.token(c.Token)
		return kCreateSessionRequest, e.buf, e.err
	case req.KillSession != nil:
		e.str(req.KillSession.SessionID)
		e.bool(req.KillSession.Remove)
		e.token(req.KillSession.Token)
		return kKillSessionRequest, e.buf, e.err
	case req.AttachSession != nil:
		a := req.AttachSession
		e.str(a.SessionID)
		e.attachmentKind(a.Kind)
		e.geometry(a.Geometry)
		if len(a.Features) > 0 {
			e.strs(a.Features)
			if a.HasFeature(CapSessionUID) {
				e.str(a.ExpectUID)
			}
			if a.HasFeature(CapAttachReplace) {
				e.str(a.Replaces)
			}
		}
		return kAttachSessionRequest, e.buf, e.err
	case req.AttachResize != nil:
		e.geometry(*req.AttachResize)
		return kAttachResizeRequest, e.buf, e.err
	case req.DetachSession != nil:
		e.str(req.DetachSession.SessionID)
		e.bool(req.DetachSession.All)
		return kDetachSessionRequest, e.buf, e.err
	case req.DetachAttachment != nil:
		e.str(req.DetachAttachment.SessionID)
		e.str(req.DetachAttachment.AttachID)
		return kDetachAttachmentRequest, e.buf, e.err
	case req.AttachInput != nil:
		e.bytes(req.AttachInput.Data)
		return kAttachInputRequest, e.buf, e.err
	case req.AttachSnapshot != nil:
		return kAttachSnapshotRequest, nil, nil
	case req.SendInput != nil:
		e.str(req.SendInput.SessionID)
		e.bytes(req.SendInput.Data)
		e.optStr(req.SendInput.SourceSessionID)
		return kSendInputRequest, e.buf, e.err
	case req.GetSession != nil:
		e.str(req.GetSession.SessionID)
		return kGetSessionRequest, e.buf, e.err
	case req.ListSessions != nil:
		return kListSessionsRequest, nil, nil
	case req.ListAttachments != nil:
		e.str(req.ListAttachments.SessionID)
		return kListAttachmentsRequest, e.buf, e.err
	case req.GetHistory != nil:
		e.str(req.GetHistory.SessionID)
		e.bool(req.GetHistory.VT)
		return kGetHistoryRequest, e.buf, e.err
	case req.ListWorkspaces != nil:
		return kListWorkspacesRequest, nil, nil
	case req.AddWorkspace != nil:
		e.str(req.AddWorkspace.Name)
		e.str(req.AddWorkspace.Path)
		e.token(req.AddWorkspace.Token)
		return kAddWorkspaceRequest, e.buf, e.err
	case req.RemoveWorkspace != nil:
		e.str(req.RemoveWorkspace.Name)
		e.token(req.RemoveWorkspace.Token)
		return kRemoveWorkspaceRequest, e.buf, e.err
	case req.Hello != nil:
		h := req.Hello
		e.u16(h.MinVersion)
		e.u16(h.MaxVersion)
		e.str(h.Client)
		e.strs(h.Capabilities)
		return kHelloRequest, e.buf, e.err
	case req.GetGitState != nil:
		e.str(req.GetGitState.SessionID)
		return kGetGitStateRequest, e.buf, e.err
	case req.ListArtifacts != nil:
		e.str(req.ListArtifacts.SessionID)
		return kListArtifactsRequest, e.buf, e.err
	case req.GetArtifact != nil:
		e.str(req.GetArtifact.SessionID)
		e.str(req.GetArtifact.Name)
		return kGetArtifactRequest, e.buf, e.err
	case req.GetSessionStats != nil:
		e.str(req.GetSessionStats.SessionID)
		e.bool(req.GetSessionStats.Snapshot)
		return kGetSessionStatsRequest, e.buf, e.err
	case req.GetDaemonStats != nil:
		return kGetDaemonStatsRequest, nil, nil
	case req.SubscribeEvents != nil:
		s := req.SubscribeEvents
		e.optU64(s.AfterID)
		e.optStr(s.SessionID)
		e.u32(s.Tail)
		return kSubscribeEventsRequest, e.buf, e.err
	case req.ListEvents != nil:
		l := req.ListEvents
		e.optU64(l.AfterID)
		e.optStr(l.SessionID)
		e.u32(l.Limit)
		return kListEventsRequest, e.buf, e.err
	case req.HandoffSession != nil:
		e.str(req.HandoffSession.SessionID)
		e.str(req.HandoffSession.Executable)
		return kHandoffSessionRequest, e.buf, e.err
	}
	return 0, nil, errors.New("empty request")
}

func decodeRequest(k kind, payload []byte, f Features) (*Request, error) {
	d := &decoder{buf: payload, f: f}
	req := &Request{}
	switch k {
	case kGetDaemonInfoRequest:
		req.GetDaemonInfo = Empty
	case kShutdownDaemonRequest:
		req.ShutdownDaemon = Empty
	case kCreateSessionRequest:
		req.CreateSession = &CreateSession{
			Cwd:       d.str(),
			Name:      d.optStr(),
			Agent:     d.str(),
			Model:     d.optStr(),
			Workspace: d.optStr(),
		}
		req.CreateSession.Token = d.token()
	case kKillSessionRequest:
		req.KillSession = &KillSession{SessionID: d.str(), Remove: d.bool()}
		req.KillSession.Token = d.token()
	case kAttachSessionRequest:
		a := &AttachSession{SessionID: d.str(), Kind: d.attachmentKind()}
		a.Geometry = d.geometry()
		// The feature list is the attach stream's handshake, so its
		// presence, not the decoder's features, says whether it is there.
		if d.more() {
			a.Features = d.strs()
			if a.HasFeature(CapSessionUID) {
				a.ExpectUID = d.str()
			}
			if a.HasFeature(CapAttachReplace) {
				a.Replaces = d.str()
			}
		}
		req.AttachSession = a
	case kAttachResizeRequest:
		g := d.geometry()
		req.AttachResize = &g
	case kDetachSessionRequest:
		req.DetachSession = &DetachSession{SessionID: d.str(), All: d.bool()}
	case kDetachAttachmentRequest:
		req.DetachAttachment = &DetachAttachment{SessionID: d.str(), AttachID: d.str()}
	case kAttachInputRequest:
		req.AttachInput = &Bytes{d.bytes()}
	case kAttachSnapshotRequest:
		req.AttachSnapshot = Empty
	case kSendInputRequest:
		req.SendInput = &SendInput{SessionID: d.str(), Data: d.bytes(), SourceSessionID: d.optStr()}
	case kGetSessionRequest:
		req.GetSession = &SessionRef{d.str()}
	case kListSessionsRequest:
		req.ListSessions = Empty
	case kListAttachmentsRequest:
		req.ListAttachments = &SessionRef{d.str()}
	case kGetHistoryRequest:
		req.GetHistory = &GetHistory{SessionID: d.str(), VT: d.bool()}
	case kListWorkspacesRequest:
		req.ListWorkspaces = Empty
	case kAddWorkspaceRequest:
		req.AddWorkspace = &AddWorkspace{Name: d.str(), Path: d.str()}
		req.AddWorkspace.Token = d.token()
	case kRemoveWorkspaceRequest:
		req.RemoveWorkspace = &WorkspaceRef{Name: d.str()}
		req.RemoveWorkspace.Token = d.token()
	case kHelloRequest:
		req.Hello = &Hello{MinVersion: d.u16(), MaxVersion: d.u16(), Client: d.str(), Capabilities: d.strs()}
	case kGetGitStateRequest:
		req.GetGitState = &SessionRef{d.str()}
	case kListArtifactsRequest:
		req.ListArtifacts = &SessionRef{d.str()}
	case kGetArtifactRequest:
		req.GetArtifact = &GetArtifact{SessionID: d.str(), Name: d.str()}
	case kGetSessionStatsRequest:
		req.GetSessionStats = &GetSessionStats{SessionID: d.str(), Snapshot: d.bool()}
	case kGetDaemonStatsRequest:
		req.GetDaemonStats = Empty
	case kSubscribeEventsRequest:
		req.SubscribeEvents = &SubscribeEvents{AfterID: d.optU64(), SessionID: d.optStr(), Tail: d.u32()}
	case kListEventsRequest:
		req.ListEvents = &ListEvents{AfterID: d.optU64(), SessionID: d.optStr(), Limit: d.u32()}
	case kHandoffSessionRequest:
		req.HandoffSession = &HandoffSession{SessionID: d.str(), Executable: d.str()}
	default:
		return nil, fmt.Errorf("unexpected message kind `%d` while decoding request", k)
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return req, nil
}

func encodeResponse(resp *Response, f Features) (kind, []byte, error) {
	e := &encoder{f: f}
	switch {
	case resp.DaemonInfo != nil:
		e.str(resp.DaemonInfo.DaemonVersion)
		e.u16(resp.DaemonInfo.ProtocolVersion)
		return kDaemonInfoResponse, e.buf, e.err
	case resp.CreateSession != nil:
		c := resp.CreateSession
		e.str(c.SessionID)
		e.str(c.Cwd)
		e.status(c.Status)
		e.mode(c.Mode)
		if e.f.Has(CapSessionUID) {
			e.str(c.UID)
		}
		return kCreateSessionResponse, e.buf, e.err
	case resp.KillSession != nil:
		e.bool(resp.KillSession.Removed)
		e.bool(resp.KillSession.WasRunning)
		return kKillSessionResponse, e.buf, e.err
	case resp.Attached != nil:
		a := resp.Attached
		e.str(a.AttachID)
		e.bytes(a.Snapshot)
		if len(a.Features) > 0 {
			e.strs(a.Features)
			if a.HasFeature(CapSessionUID) {
				e.str(a.SessionUID)
			}
		}
		return kAttachedResponse, e.buf, e.err
	case resp.AttachSnapshot != nil:
		e.bytes(resp.AttachSnapshot.Data)
		return kAttachSnapshotResponse, e.buf, e.err
	case resp.SessionEnded != nil:
		s := resp.SessionEnded
		e.str(s.SessionID)
		e.status(s.Status)
		e.optI32(s.ExitCode)
		e.optStr(s.Error)
		return kSessionEndedResponse, e.buf, e.err
	case resp.InputAccepted != nil:
		return kInputAcceptedResponse, nil, nil
	case resp.Session != nil:
		e.sessionRecord(resp.Session)
		return kSessionResponse, e.buf, e.err
	case resp.Sessions != nil:
		e.length(len(*resp.Sessions))
		for i := range *resp.Sessions {
			e.sessionRecord(&(*resp.Sessions)[i])
		}
		return kSessionsResponse, e.buf, e.err
	case resp.Attachments != nil:
		e.length(len(*resp.Attachments))
		for _, a := range *resp.Attachments {
			e.str(a.AttachID)
			e.str(a.SessionID)
			e.attachmentKind(a.Kind)
			e.datetime(a.ConnectedAt)
		}
		return kAttachmentsResponse, e.buf, e.err
	case resp.History != nil:
		e.str(resp.History.Data)
		return kHistoryResponse, e.buf, e.err
	case resp.PtyOutput != nil:
		e.bytes(resp.PtyOutput.Data)
		return kPtyOutputResponse, e.buf, e.err
	case resp.EndOfStream != nil:
		return kEndOfStreamResponse, nil, nil
	case resp.Error != nil:
		e.str(resp.Error.Message)
		return kErrorResponse, e.buf, e.err
	case resp.Ok != nil:
		return kOkResponse, nil, nil
	case resp.Workspaces != nil:
		e.length(len(*resp.Workspaces))
		for i := range *resp.Workspaces {
			e.workspace(&(*resp.Workspaces)[i])
		}
		return kWorkspacesResponse, e.buf, e.err
	case resp.Workspace != nil:
		e.workspace(resp.Workspace)
		return kWorkspaceResponse, e.buf, e.err
	case resp.Welcome != nil:
		w := resp.Welcome
		e.u16(w.Version)
		e.str(w.DaemonVersion)
		e.strs(w.Capabilities)
		e.str(w.Host.Name)
		e.str(w.Host.OS)
		e.str(w.Host.Arch)
		e.u32(w.Host.CPUs)
		e.u64(w.Host.MemoryBytes)
		e.strs(w.Host.Agents)
		e.str(w.Host.DefaultAgent)
		// Appended (CapDaemonID); set only for a client whose Hello
		// listed the capability, so older clients never see it.
		if w.DaemonID != "" {
			e.str(w.DaemonID)
		}
		return kWelcomeResponse, e.buf, e.err
	case resp.GitState != nil:
		e.gitState(resp.GitState)
		return kGitStateResponse, e.buf, e.err
	case resp.Artifacts != nil:
		e.length(len(*resp.Artifacts))
		for _, a := range *resp.Artifacts {
			e.str(a.Name)
			e.str(a.Kind)
			e.str(a.Description)
			e.optU64(a.Size)
		}
		return kArtifactsResponse, e.buf, e.err
	case resp.ArtifactChunk != nil:
		e.bytes(resp.ArtifactChunk.Data)
		return kArtifactChunkResponse, e.buf, e.err
	case resp.SessionStats != nil:
		e.sessionStats(resp.SessionStats)
		return kSessionStatsResponse, e.buf, e.err
	case resp.DaemonStats != nil:
		e.processStats(&resp.DaemonStats.Daemon)
		e.u32(resp.DaemonStats.OpenStreams)
		return kDaemonStatsResponse, e.buf, e.err
	case resp.AttachResync != nil:
		e.bytes(resp.AttachResync.Data)
		return kAttachResyncResponse, e.buf, e.err
	case resp.Event != nil:
		e.event(resp.Event)
		return kEventResponse, e.buf, e.err
	case resp.Events != nil:
		e.length(len(*resp.Events))
		for i := range *resp.Events {
			e.event(&(*resp.Events)[i])
		}
		return kEventsResponse, e.buf, e.err
	case resp.HandedOff != nil:
		h := resp.HandedOff
		e.str(h.SessionID)
		e.u32(h.WorkerPID)
		e.str(h.Executable)
		e.u32(h.Handoffs)
		return kHandedOffResponse, e.buf, e.err
	case resp.SessionRestarting != nil:
		e.str(resp.SessionRestarting.SessionID)
		e.str(resp.SessionRestarting.Reason)
		return kSessionRestartingResponse, e.buf, e.err
	}
	return 0, nil, errors.New("empty response")
}

func decodeResponse(k kind, payload []byte, f Features) (*Response, error) {
	d := &decoder{buf: payload, f: f}
	resp := &Response{}
	switch k {
	case kDaemonInfoResponse:
		resp.DaemonInfo = &DaemonInfo{DaemonVersion: d.str(), ProtocolVersion: d.u16()}
	case kCreateSessionResponse:
		resp.CreateSession = &session.CreateResult{
			SessionID: d.str(), Cwd: d.str(), Status: d.status(), Mode: d.mode(),
		}
		if d.f.Has(CapSessionUID) {
			resp.CreateSession.UID = d.str()
		}
	case kKillSessionResponse:
		resp.KillSession = &KillSessionResult{Removed: d.bool(), WasRunning: d.bool()}
	case kAttachedResponse:
		a := &Attached{AttachID: d.str(), Snapshot: d.bytes()}
		if d.more() {
			a.Features = d.strs()
			if a.HasFeature(CapSessionUID) {
				a.SessionUID = d.str()
			}
		}
		resp.Attached = a
	case kAttachSnapshotResponse:
		resp.AttachSnapshot = &Bytes{d.bytes()}
	case kSessionEndedResponse:
		resp.SessionEnded = &SessionEnded{
			SessionID: d.str(), Status: d.status(), ExitCode: d.optI32(), Error: d.optStr(),
		}
	case kInputAcceptedResponse:
		resp.InputAccepted = Empty
	case kSessionResponse:
		rec := d.sessionRecord()
		resp.Session = &rec
	case kSessionsResponse:
		n := d.length()
		sessions := make([]session.Record, 0, d.capacity(n))
		for i := 0; i < n && d.err == nil; i++ {
			sessions = append(sessions, d.sessionRecord())
		}
		resp.Sessions = &sessions
	case kAttachmentsResponse:
		n := d.length()
		attachments := make([]session.AttachmentRecord, 0, d.capacity(n))
		for i := 0; i < n && d.err == nil; i++ {
			attachments = append(attachments, session.AttachmentRecord{
				AttachID: d.str(), SessionID: d.str(), Kind: d.attachmentKind(), ConnectedAt: d.datetime(),
			})
		}
		resp.Attachments = &attachments
	case kHistoryResponse:
		resp.History = &History{Data: d.str()}
	case kPtyOutputResponse:
		resp.PtyOutput = &Bytes{d.bytes()}
	case kEndOfStreamResponse:
		resp.EndOfStream = Empty
	case kErrorResponse:
		resp.Error = &ErrorResponse{Message: d.str()}
	case kOkResponse:
		resp.Ok = Empty
	case kWorkspacesResponse:
		n := d.length()
		workspaces := make([]session.Workspace, 0, d.capacity(n))
		for i := 0; i < n && d.err == nil; i++ {
			workspaces = append(workspaces, d.workspace())
		}
		resp.Workspaces = &workspaces
	case kWorkspaceResponse:
		w := d.workspace()
		resp.Workspace = &w
	case kWelcomeResponse:
		w := &Welcome{Version: d.u16(), DaemonVersion: d.str(), Capabilities: d.strs()}
		w.Host = HostInfo{
			Name: d.str(), OS: d.str(), Arch: d.str(), CPUs: d.u32(), MemoryBytes: d.u64(),
			Agents: d.strs(), DefaultAgent: d.str(),
		}
		if d.more() {
			w.DaemonID = d.str()
		}
		resp.Welcome = w
	case kGitStateResponse:
		g := d.gitState()
		resp.GitState = &g
	case kArtifactsResponse:
		n := d.length()
		artifacts := make([]session.Artifact, 0, d.capacity(n))
		for i := 0; i < n && d.err == nil; i++ {
			artifacts = append(artifacts, session.Artifact{Name: d.str(), Kind: d.str(), Description: d.str(), Size: d.optU64()})
		}
		resp.Artifacts = &artifacts
	case kArtifactChunkResponse:
		resp.ArtifactChunk = &Bytes{d.bytes()}
	case kSessionStatsResponse:
		resp.SessionStats = d.sessionStats()
	case kDaemonStatsResponse:
		resp.DaemonStats = &DaemonStats{Daemon: d.processStats(), OpenStreams: d.u32()}
	case kAttachResyncResponse:
		resp.AttachResync = &Bytes{d.bytes()}
	case kEventResponse:
		ev := d.event()
		resp.Event = &ev
	case kEventsResponse:
		n := d.length()
		events := make([]session.Event, 0, d.capacity(n))
		for i := 0; i < n && d.err == nil; i++ {
			events = append(events, d.event())
		}
		resp.Events = &events
	case kHandedOffResponse:
		resp.HandedOff = &HandedOff{SessionID: d.str(), WorkerPID: d.u32(), Executable: d.str(), Handoffs: d.u32()}
	case kSessionRestartingResponse:
		resp.SessionRestarting = &SessionRestarting{SessionID: d.str(), Reason: d.str()}
	default:
		return nil, fmt.Errorf("unexpected message kind `%d` while decoding response", k)
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Primitive encoders

type encoder struct {
	buf []byte
	err error
	// f decides which optional fields are written.
	f Features
}

func (e *encoder) u8(v uint8)   { e.buf = append(e.buf, v) }
func (e *encoder) bool(v bool)  { e.u8(map[bool]uint8{false: 0, true: 1}[v]) }
func (e *encoder) u16(v uint16) { e.buf = binary.LittleEndian.AppendUint16(e.buf, v) }
func (e *encoder) u32(v uint32) { e.buf = binary.LittleEndian.AppendUint32(e.buf, v) }
func (e *encoder) i32(v int32)  { e.u32(uint32(v)) }
func (e *encoder) i64(v int64)  { e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(v)) }
func (e *encoder) u64(v uint64) { e.buf = binary.LittleEndian.AppendUint64(e.buf, v) }

func (e *encoder) length(n int) {
	if n < 0 || uint64(n) > uint64(^uint32(0)) {
		e.err = errors.New("length exceeds u32")
		return
	}
	e.u32(uint32(n))
}

func (e *encoder) bytes(v []byte) {
	e.length(len(v))
	e.buf = append(e.buf, v...)
}

func (e *encoder) str(v string) { e.bytes([]byte(v)) }

func (e *encoder) strs(v []string) {
	e.length(len(v))
	for _, s := range v {
		e.str(s)
	}
}

func (e *encoder) optStr(v *string) {
	if v == nil {
		e.u8(0)
		return
	}
	e.u8(1)
	e.str(*v)
}

func (e *encoder) optU32(v *uint32) {
	if v == nil {
		e.u8(0)
		return
	}
	e.u8(1)
	e.u32(*v)
}

func (e *encoder) optU64(v *uint64) {
	if v == nil {
		e.u8(0)
		return
	}
	e.u8(1)
	e.u64(*v)
}

func (e *encoder) optI32(v *int32) {
	if v == nil {
		e.u8(0)
		return
	}
	e.u8(1)
	e.i32(*v)
}

func (e *encoder) geometry(g Geometry) {
	e.u16(g.Cols)
	e.u16(g.Rows)
	e.u16(g.PixelWidth)
	e.u16(g.PixelHeight)
}

func (e *encoder) datetime(t time.Time) {
	e.i64(t.Unix())
	e.u32(uint32(t.Nanosecond()))
}

func (e *encoder) optDatetime(t *time.Time) {
	if t == nil {
		e.u8(0)
		return
	}
	e.u8(1)
	e.datetime(*t)
}

func (e *encoder) status(s session.Status) {
	switch s {
	case session.StatusCreating:
		e.u8(1)
	case session.StatusRunning:
		e.u8(2)
	case session.StatusExited:
		e.u8(3)
	case session.StatusFailed:
		e.u8(4)
	case session.StatusUnknownRecovered:
		e.u8(5)
	default:
		e.err = fmt.Errorf("invalid session status %q", s)
	}
}

func (e *encoder) attachmentKind(k session.AttachmentKind) {
	switch k {
	case session.AttachmentAttach:
		e.u8(1)
	case session.AttachmentTui:
		e.u8(2)
	default:
		e.err = fmt.Errorf("invalid attachment kind %q", k)
	}
}

func (e *encoder) attention(a session.AttentionLevel) {
	switch a {
	case session.AttentionInfo:
		e.u8(1)
	case session.AttentionNotice:
		e.u8(2)
	case session.AttentionAction:
		e.u8(3)
	default:
		e.err = fmt.Errorf("invalid attention level %q", a)
	}
}

func (e *encoder) mode(m session.Mode) {
	switch m {
	case session.ModeExecute:
		e.u8(1)
	case session.ModePlan:
		e.u8(2)
	default:
		e.err = fmt.Errorf("invalid session mode %q", m)
	}
}

func (e *encoder) sessionRecord(s *session.Record) {
	e.str(s.SessionID)
	e.str(s.Agent)
	e.optStr(s.Model)
	e.mode(s.Mode)
	e.str(s.Cwd)
	e.status(s.Status)
	e.optU32(s.WorkerPID)
	e.optU32(s.AgentPID)
	e.optI32(s.ExitCode)
	e.optStr(s.Error)
	e.attention(s.Attention)
	e.optStr(s.AttentionSummary)
	e.datetime(s.CreatedAt)
	e.datetime(s.UpdatedAt)
	e.optDatetime(s.ExitedAt)
	e.optStr(s.Workspace)
	if e.f.Has(CapSessionUID) {
		e.str(s.UID)
	}
	if e.f.Has(CapSessionActivity) {
		e.str(string(s.Activity))
		e.optStr(s.Foreground)
		e.optStr(s.Title)
		e.optDatetime(s.LastOutputAt)
		e.optDatetime(s.AttentionAt)
	}
}

// token writes a request token (CapRequestTokens).
func (e *encoder) token(t string) {
	if e.f.Has(CapRequestTokens) {
		e.str(t)
	}
}

// event encodes an event. Its kind is a string, so kinds added later reach
// older clients as text they can show rather than a value they cannot
// decode.
func (e *encoder) event(ev *session.Event) {
	e.u64(ev.ID)
	e.str(ev.SessionID)
	e.datetime(ev.At)
	e.str(string(ev.Kind))
	e.attention(ev.Attention)
	e.str(ev.Summary)
}

func (e *encoder) workspace(w *session.Workspace) {
	e.str(w.Name)
	e.str(w.Path)
	e.datetime(w.CreatedAt)
}

// ---------------------------------------------------------------------------
// Primitive decoders

type decoder struct {
	buf []byte
	pos int
	err error
	// f decides which optional fields are expected.
	f Features
}

func (d *decoder) fail(format string, args ...any) {
	if d.err == nil {
		d.err = fmt.Errorf(format, args...)
	}
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || d.pos+n > len(d.buf) {
		d.fail("truncated payload")
		return nil
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b
}

// capacity bounds a slice's initial capacity for a count read off the wire:
// every element takes at least one byte, so no more than the bytes left can
// be real, and a lying count cannot make the decoder allocate gigabytes.
func (d *decoder) capacity(n int) int {
	return max(0, min(n, len(d.buf)-d.pos))
}

// more reports whether bytes remain. A field appended to the end of a
// message (never to a struct inside a list) may be decoded only if present,
// for peers that send it unconditionally once the other side has said, in
// some earlier frame, that it understands it.
func (d *decoder) more() bool {
	return d.err == nil && d.pos < len(d.buf)
}

func (d *decoder) finish() error {
	if d.err != nil {
		return d.err
	}
	if d.pos != len(d.buf) {
		return errors.New("unexpected trailing payload bytes")
	}
	return nil
}

func (d *decoder) u8() uint8 {
	b := d.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (d *decoder) u16() uint16 {
	b := d.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (d *decoder) u32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (d *decoder) i32() int32 { return int32(d.u32()) }

func (d *decoder) u64() uint64 { return uint64(d.i64()) }

func (d *decoder) i64() int64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(b))
}

func (d *decoder) bool() bool {
	switch d.u8() {
	case 0:
		return false
	case 1:
		return true
	default:
		if d.err == nil {
			d.fail("invalid bool value")
		}
		return false
	}
}

func (d *decoder) length() int { return int(d.u32()) }

func (d *decoder) bytes() []byte {
	n := d.length()
	b := d.take(n)
	if b == nil {
		return nil
	}
	out := make([]byte, n)
	copy(out, b)
	return out
}

// str rejects invalid UTF-8, so that nothing the daemon accepts (and may
// persist) is text the CLI would print as something else.
func (d *decoder) str() string {
	b := d.bytes()
	if !utf8.Valid(b) {
		d.fail("invalid UTF-8 in string field")
		return ""
	}
	return string(b)
}

// strs decodes a list of strings. The count comes off the wire, so the slice
// grows as elements decode rather than being sized from it up front.
func (d *decoder) strs() []string {
	n := d.length()
	var out []string
	for i := 0; i < n && d.err == nil; i++ {
		out = append(out, d.str())
	}
	return out
}

func (d *decoder) optStr() *string {
	if !d.bool() {
		return nil
	}
	s := d.str()
	return &s
}

func (d *decoder) optU32() *uint32 {
	if !d.bool() {
		return nil
	}
	v := d.u32()
	return &v
}

func (d *decoder) optU64() *uint64 {
	if !d.bool() {
		return nil
	}
	v := d.u64()
	return &v
}

func (d *decoder) optI32() *int32 {
	if !d.bool() {
		return nil
	}
	v := d.i32()
	return &v
}

func (d *decoder) geometry() Geometry {
	return Geometry{Cols: d.u16(), Rows: d.u16(), PixelWidth: d.u16(), PixelHeight: d.u16()}
}

func (d *decoder) datetime() time.Time {
	secs := d.i64()
	nanos := d.u32()
	if nanos >= 2_000_000_000 {
		d.fail("invalid UTC timestamp")
	}
	return time.Unix(secs, int64(nanos)).UTC()
}

func (d *decoder) optDatetime() *time.Time {
	if !d.bool() {
		return nil
	}
	t := d.datetime()
	return &t
}

func (d *decoder) status() session.Status {
	switch v := d.u8(); v {
	case 1:
		return session.StatusCreating
	case 2:
		return session.StatusRunning
	case 3:
		return session.StatusExited
	case 4:
		return session.StatusFailed
	case 5:
		return session.StatusUnknownRecovered
	default:
		d.fail("invalid session status `%d`", v)
		return ""
	}
}

func (d *decoder) attachmentKind() session.AttachmentKind {
	switch v := d.u8(); v {
	case 1:
		return session.AttachmentAttach
	case 2:
		return session.AttachmentTui
	default:
		d.fail("invalid attachment kind `%d`", v)
		return ""
	}
}

func (d *decoder) attention() session.AttentionLevel {
	switch v := d.u8(); v {
	case 1:
		return session.AttentionInfo
	case 2:
		return session.AttentionNotice
	case 3:
		return session.AttentionAction
	default:
		d.fail("invalid attention level `%d`", v)
		return ""
	}
}

func (d *decoder) mode() session.Mode {
	switch v := d.u8(); v {
	case 1:
		return session.ModeExecute
	case 2:
		return session.ModePlan
	default:
		d.fail("invalid session mode `%d`", v)
		return ""
	}
}

func (d *decoder) sessionRecord() session.Record {
	rec := session.Record{
		SessionID:        d.str(),
		Agent:            d.str(),
		Model:            d.optStr(),
		Mode:             d.mode(),
		Cwd:              d.str(),
		Status:           d.status(),
		WorkerPID:        d.optU32(),
		AgentPID:         d.optU32(),
		ExitCode:         d.optI32(),
		Error:            d.optStr(),
		Attention:        d.attention(),
		AttentionSummary: d.optStr(),
		CreatedAt:        d.datetime(),
		UpdatedAt:        d.datetime(),
		ExitedAt:         d.optDatetime(),
		Workspace:        d.optStr(),
	}
	if d.f.Has(CapSessionUID) {
		rec.UID = d.str()
	}
	if d.f.Has(CapSessionActivity) {
		rec.Activity = session.Activity(d.str())
		rec.Foreground = d.optStr()
		rec.Title = d.optStr()
		rec.LastOutputAt = d.optDatetime()
		rec.AttentionAt = d.optDatetime()
	}
	return rec
}

// token reads a request token (CapRequestTokens).
func (d *decoder) token() string {
	if d.f.Has(CapRequestTokens) {
		return d.str()
	}
	return ""
}

func (d *decoder) event() session.Event {
	return session.Event{
		ID: d.u64(), SessionID: d.str(), At: d.datetime(), Kind: session.EventKind(d.str()),
		Attention: d.attention(), Summary: d.str(),
	}
}

func (d *decoder) workspace() session.Workspace {
	return session.Workspace{Name: d.str(), Path: d.str(), CreatedAt: d.datetime()}
}
