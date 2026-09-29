// Package protocol implements the framed binary wire protocol shared by the
// agent CLI, the daemon, and session workers.
//
// crates/agentd-shared/src/protocol.rs implements the same protocol for the
// agent CLI; the two must agree byte for byte (golden-frame tests on both
// sides pin this).
package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/robmorgan/agentd/go/internal/session"
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
	// retained scrollback. crates/agentd-shared uses the same limit.
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
}

// AddWorkspace registers Path under Name. Path is resolved on the daemon's
// machine: it must be absolute or start with `~/`, and exist.
type AddWorkspace struct {
	Name, Path string
}

type WorkspaceRef struct{ Name string }

type KillSession struct {
	SessionID string
	Remove    bool
}

type Geometry struct {
	Cols, Rows, PixelWidth, PixelHeight uint16
}

type AttachSession struct {
	SessionID string
	Kind      session.AttachmentKind
	Geometry
}

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
}

type DaemonInfo struct {
	DaemonVersion   string
	ProtocolVersion uint16
}

type KillSessionResult struct{ Removed, WasRunning bool }
type Attached struct {
	AttachID string
	Snapshot []byte
}
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
)

// ---------------------------------------------------------------------------
// Framing

type frameHeader struct {
	version uint16
	kind    uint16
}

func writeFrame(w io.Writer, version uint16, k uint16, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("frame payload too large: %d bytes (limit %d)", len(payload), MaxFramePayload)
	}
	var header [frameHeaderLen]byte
	binary.LittleEndian.PutUint32(header[0:4], frameMagic)
	binary.LittleEndian.PutUint16(header[4:6], version)
	binary.LittleEndian.PutUint16(header[6:8], k)
	binary.LittleEndian.PutUint16(header[8:10], 0)
	binary.LittleEndian.PutUint16(header[10:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(payload)))
	buf := make([]byte, 0, frameHeaderLen+len(payload))
	buf = append(buf, header[:]...)
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
	payloadLen := binary.LittleEndian.Uint32(header[12:16])
	if payloadLen > MaxFramePayload {
		return nil, nil, fmt.Errorf("frame payload too large: %d bytes (limit %d)", payloadLen, MaxFramePayload)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, nil, err
	}
	return h, payload, nil
}

// readStandardFrame returns ok=false on a clean EOF before any frame.
func readStandardFrame(r io.Reader) (k kind, payload []byte, ok bool, err error) {
	h, payload, err := readRawFrame(r)
	if err != nil || h == nil {
		return 0, nil, false, err
	}
	if h.version != ProtocolVersion {
		return 0, nil, false, &VersionError{Version: h.version}
	}
	return kind(h.kind), payload, true, nil
}

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

func WriteRequest(w io.Writer, req *Request) error {
	k, payload, err := encodeRequest(req)
	if err != nil {
		return err
	}
	return writeFrame(w, ProtocolVersion, uint16(k), payload)
}

func WriteResponse(w io.Writer, resp *Response) error {
	k, payload, err := encodeResponse(resp)
	if err != nil {
		return err
	}
	return writeFrame(w, ProtocolVersion, uint16(k), payload)
}

// ReadRequest returns (nil, nil) when the peer closed the connection cleanly.
func ReadRequest(r io.Reader) (*Request, error) {
	k, payload, ok, err := readStandardFrame(r)
	if err != nil || !ok {
		return nil, err
	}
	return decodeRequest(k, payload)
}

// ReadResponse returns (nil, nil) when the peer closed the connection cleanly.
func ReadResponse(r io.Reader) (*Response, error) {
	k, payload, ok, err := readStandardFrame(r)
	if err != nil || !ok {
		return nil, err
	}
	return decodeResponse(k, payload)
}

// ---------------------------------------------------------------------------
// Encoding

func encodeRequest(req *Request) (kind, []byte, error) {
	e := &encoder{}
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
		return kCreateSessionRequest, e.buf, e.err
	case req.KillSession != nil:
		e.str(req.KillSession.SessionID)
		e.bool(req.KillSession.Remove)
		return kKillSessionRequest, e.buf, e.err
	case req.AttachSession != nil:
		a := req.AttachSession
		e.str(a.SessionID)
		e.attachmentKind(a.Kind)
		e.geometry(a.Geometry)
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
		return kAddWorkspaceRequest, e.buf, e.err
	case req.RemoveWorkspace != nil:
		e.str(req.RemoveWorkspace.Name)
		return kRemoveWorkspaceRequest, e.buf, e.err
	}
	return 0, nil, errors.New("empty request")
}

func decodeRequest(k kind, payload []byte) (*Request, error) {
	d := &decoder{buf: payload}
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
	case kKillSessionRequest:
		req.KillSession = &KillSession{SessionID: d.str(), Remove: d.bool()}
	case kAttachSessionRequest:
		a := &AttachSession{SessionID: d.str(), Kind: d.attachmentKind()}
		a.Geometry = d.geometry()
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
	case kRemoveWorkspaceRequest:
		req.RemoveWorkspace = &WorkspaceRef{Name: d.str()}
	default:
		return nil, fmt.Errorf("unexpected message kind `%d` while decoding request", k)
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return req, nil
}

func encodeResponse(resp *Response) (kind, []byte, error) {
	e := &encoder{}
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
		return kCreateSessionResponse, e.buf, e.err
	case resp.KillSession != nil:
		e.bool(resp.KillSession.Removed)
		e.bool(resp.KillSession.WasRunning)
		return kKillSessionResponse, e.buf, e.err
	case resp.Attached != nil:
		e.str(resp.Attached.AttachID)
		e.bytes(resp.Attached.Snapshot)
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
	}
	return 0, nil, errors.New("empty response")
}

func decodeResponse(k kind, payload []byte) (*Response, error) {
	d := &decoder{buf: payload}
	resp := &Response{}
	switch k {
	case kDaemonInfoResponse:
		resp.DaemonInfo = &DaemonInfo{DaemonVersion: d.str(), ProtocolVersion: d.u16()}
	case kCreateSessionResponse:
		resp.CreateSession = &session.CreateResult{
			SessionID: d.str(), Cwd: d.str(), Status: d.status(), Mode: d.mode(),
		}
	case kKillSessionResponse:
		resp.KillSession = &KillSessionResult{Removed: d.bool(), WasRunning: d.bool()}
	case kAttachedResponse:
		resp.Attached = &Attached{AttachID: d.str(), Snapshot: d.bytes()}
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
		sessions := make([]session.Record, 0, n)
		for i := 0; i < n && d.err == nil; i++ {
			sessions = append(sessions, d.sessionRecord())
		}
		resp.Sessions = &sessions
	case kAttachmentsResponse:
		n := d.length()
		attachments := make([]session.AttachmentRecord, 0, n)
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
		workspaces := make([]session.Workspace, 0, n)
		for i := 0; i < n && d.err == nil; i++ {
			workspaces = append(workspaces, d.workspace())
		}
		resp.Workspaces = &workspaces
	case kWorkspaceResponse:
		w := d.workspace()
		resp.Workspace = &w
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
}

func (e *encoder) u8(v uint8)   { e.buf = append(e.buf, v) }
func (e *encoder) bool(v bool)  { e.u8(map[bool]uint8{false: 0, true: 1}[v]) }
func (e *encoder) u16(v uint16) { e.buf = binary.LittleEndian.AppendUint16(e.buf, v) }
func (e *encoder) u32(v uint32) { e.buf = binary.LittleEndian.AppendUint32(e.buf, v) }
func (e *encoder) i32(v int32)  { e.u32(uint32(v)) }
func (e *encoder) i64(v int64)  { e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(v)) }

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

// str rejects invalid UTF-8, as the Rust decoder does, so that nothing the Go
// side accepts (and may persist) can later fail to decode in the CLI.
func (d *decoder) str() string {
	b := d.bytes()
	if !utf8.Valid(b) {
		d.fail("invalid UTF-8 in string field")
		return ""
	}
	return string(b)
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
	return session.Record{
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
}

func (d *decoder) workspace() session.Workspace {
	return session.Workspace{Name: d.str(), Path: d.str(), CreatedAt: d.datetime()}
}
