use anyhow::{Context, Result, anyhow, bail, ensure};
use chrono::{DateTime, TimeZone, Utc};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use crate::session::{
    AttachmentKind, AttachmentRecord, AttentionLevel, CreateSessionResult, SessionMode,
    SessionRecord, SessionStatus,
};

/// Version 33 drops the worktree, apply, discard and diff messages and the git
/// fields on session records (see docs/drop-worktrees.md). The encoding must
/// stay byte for byte identical to go/internal/protocol/protocol.go.
pub const PROTOCOL_VERSION: u16 = 33;
pub const DAEMON_MANAGEMENT_VERSION: u16 = 1;

/// Largest payload a frame may declare or carry. Readers reject larger
/// declared lengths before allocating, so a corrupt or hostile header cannot
/// make us allocate up to 4 GiB. Must match go/internal/protocol.
pub const MAX_FRAME_PAYLOAD: usize = 64 * 1024 * 1024;

const FRAME_MAGIC: u32 = 0x4147_4450;
const FRAME_HEADER_LEN: usize = 16;
const DAEMON_STATUS_REQUEST_KIND: u16 = 20_001;
const DAEMON_SHUTDOWN_REQUEST_KIND: u16 = 20_002;
const DAEMON_STATUS_RESPONSE_KIND: u16 = 21_001;
const DAEMON_SHUTDOWN_RESPONSE_KIND: u16 = 21_002;
const DAEMON_ERROR_RESPONSE_KIND: u16 = 21_099;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DaemonInfo {
    pub daemon_version: String,
    pub protocol_version: u16,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DaemonManagementStatus {
    pub daemon_version: String,
    pub protocol_version: u16,
    pub pid: u32,
    pub root: String,
    pub socket: String,
    pub running_sessions: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub enum DaemonManagementRequest {
    Status,
    Shutdown { force: bool },
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub enum DaemonManagementResponse {
    Status { status: DaemonManagementStatus },
    Shutdown { stopped: bool, running_sessions: bool, message: String },
    Error { message: String },
}

#[derive(Debug, Clone, PartialEq)]
pub enum IncomingRequest {
    Standard(Request),
    DaemonManagement(DaemonManagementRequest),
}

#[derive(Debug, Clone, PartialEq)]
pub enum Request {
    GetDaemonInfo,
    ShutdownDaemon,
    CreateSession {
        cwd: String,
        name: Option<String>,
        agent: String,
        model: Option<String>,
    },
    KillSession {
        session_id: String,
        remove: bool,
        force: bool,
    },
    ResolveSessionRuntime {
        session_id: String,
    },
    AttachSession {
        session_id: String,
        kind: AttachmentKind,
        cols: u16,
        rows: u16,
        pixel_width: u16,
        pixel_height: u16,
    },
    AttachResize {
        cols: u16,
        rows: u16,
        pixel_width: u16,
        pixel_height: u16,
    },
    DetachSession {
        session_id: String,
        all: bool,
    },
    DetachAttachment {
        session_id: String,
        attach_id: String,
    },
    AttachInput {
        data: Vec<u8>,
    },
    AttachSnapshot,
    SendInput {
        session_id: String,
        data: Vec<u8>,
        source_session_id: Option<String>,
    },
    SwitchAttachedSession {
        source_session_id: String,
        target_session_id: String,
    },
    GetSession {
        session_id: String,
    },
    ListSessions,
    ListAttachments {
        session_id: String,
    },
    GetHistory {
        session_id: String,
        vt: bool,
    },
}

#[derive(Debug, Clone, PartialEq)]
pub enum Response {
    DaemonInfo {
        info: DaemonInfo,
    },
    CreateSession {
        session: CreateSessionResult,
    },
    KillSession {
        removed: bool,
        was_running: bool,
    },
    RuntimeEndpoint {
        socket_path: String,
    },
    Attached {
        attach_id: String,
        snapshot: Vec<u8>,
    },
    AttachSnapshot {
        snapshot: Vec<u8>,
    },
    SessionEnded {
        session_id: String,
        status: SessionStatus,
        exit_code: Option<i32>,
        error: Option<String>,
    },
    InputAccepted,
    Session {
        session: SessionRecord,
    },
    Sessions {
        sessions: Vec<SessionRecord>,
    },
    Attachments {
        attachments: Vec<AttachmentRecord>,
    },
    History {
        data: String,
    },
    PtyOutput {
        data: Vec<u8>,
    },
    SwitchSession {
        session_id: String,
    },
    EndOfStream,
    Error {
        message: String,
    },
    Ok,
}

// Kind numbers are stable across versions. Numbers removed in v33 (4, 5, 10,
// 19, 20, 106, 107: worktree, diff, apply and discard) are left unassigned so a
// v32 frame can never be misread as something else; 10 and 107 are reserved
// for a future cwd-based diff.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u16)]
enum MessageKind {
    GetDaemonInfoRequest = 1,
    ShutdownDaemonRequest = 2,
    CreateSessionRequest = 3,
    KillSessionRequest = 6,
    ResolveSessionRuntimeRequest = 25,
    AttachSessionRequest = 7,
    AttachInputRequest = 8,
    AttachSnapshotRequest = 24,
    SendInputRequest = 9,
    ListAttachmentsRequest = 22,
    DetachAttachmentRequest = 23,
    AttachResizeRequest = 21,
    DetachSessionRequest = 17,
    SwitchAttachedSessionRequest = 16,
    GetSessionRequest = 11,
    ListSessionsRequest = 12,
    GetHistoryRequest = 14,
    DaemonInfoResponse = 101,
    CreateSessionResponse = 102,
    KillSessionResponse = 103,
    RuntimeEndpointResponse = 120,
    AttachedResponse = 104,
    AttachSnapshotResponse = 119,
    SessionEndedResponse = 117,
    AttachmentsResponse = 118,
    InputAcceptedResponse = 105,
    SessionResponse = 108,
    SessionsResponse = 109,
    HistoryResponse = 111,
    PtyOutputResponse = 112,
    SwitchSessionResponse = 116,
    EndOfStreamResponse = 113,
    ErrorResponse = 114,
    OkResponse = 115,
}

impl MessageKind {
    fn from_u16(value: u16) -> Result<Self> {
        Ok(match value {
            1 => Self::GetDaemonInfoRequest,
            2 => Self::ShutdownDaemonRequest,
            3 => Self::CreateSessionRequest,
            6 => Self::KillSessionRequest,
            25 => Self::ResolveSessionRuntimeRequest,
            7 => Self::AttachSessionRequest,
            8 => Self::AttachInputRequest,
            24 => Self::AttachSnapshotRequest,
            9 => Self::SendInputRequest,
            22 => Self::ListAttachmentsRequest,
            23 => Self::DetachAttachmentRequest,
            21 => Self::AttachResizeRequest,
            17 => Self::DetachSessionRequest,
            16 => Self::SwitchAttachedSessionRequest,
            11 => Self::GetSessionRequest,
            12 => Self::ListSessionsRequest,
            14 => Self::GetHistoryRequest,
            101 => Self::DaemonInfoResponse,
            102 => Self::CreateSessionResponse,
            103 => Self::KillSessionResponse,
            120 => Self::RuntimeEndpointResponse,
            104 => Self::AttachedResponse,
            119 => Self::AttachSnapshotResponse,
            117 => Self::SessionEndedResponse,
            118 => Self::AttachmentsResponse,
            105 => Self::InputAcceptedResponse,
            108 => Self::SessionResponse,
            109 => Self::SessionsResponse,
            111 => Self::HistoryResponse,
            112 => Self::PtyOutputResponse,
            116 => Self::SwitchSessionResponse,
            113 => Self::EndOfStreamResponse,
            114 => Self::ErrorResponse,
            115 => Self::OkResponse,
            other => bail!("unknown message kind `{other}`"),
        })
    }
}

pub async fn write_request<W>(writer: &mut W, request: &Request) -> Result<()>
where
    W: AsyncWrite + Unpin,
{
    let (kind, payload) = encode_request(request)?;
    write_frame(writer, PROTOCOL_VERSION, kind as u16, &payload).await
}

pub async fn write_response<W>(writer: &mut W, response: &Response) -> Result<()>
where
    W: AsyncWrite + Unpin,
{
    let (kind, payload) = encode_response(response)?;
    write_frame(writer, PROTOCOL_VERSION, kind as u16, &payload).await
}

pub async fn read_request<R>(reader: &mut R) -> Result<Option<Request>>
where
    R: AsyncRead + Unpin,
{
    let Some((kind, payload)) = read_standard_frame(reader).await? else {
        return Ok(None);
    };
    decode_request(kind, &payload).map(Some)
}

pub async fn read_response<R>(reader: &mut R) -> Result<Option<Response>>
where
    R: AsyncRead + Unpin,
{
    let Some((kind, payload)) = read_standard_frame(reader).await? else {
        return Ok(None);
    };
    decode_response(kind, &payload).map(Some)
}

pub async fn read_incoming_request<R>(reader: &mut R) -> Result<Option<IncomingRequest>>
where
    R: AsyncRead + Unpin,
{
    let Some((header, payload)) = read_raw_frame(reader).await? else {
        return Ok(None);
    };

    if header.version == DAEMON_MANAGEMENT_VERSION {
        let request = decode_daemon_management_request(header.kind, &payload)?;
        return Ok(Some(IncomingRequest::DaemonManagement(request)));
    }

    ensure!(
        header.version == PROTOCOL_VERSION,
        "unsupported protocol version `{}`",
        header.version
    );
    let kind = MessageKind::from_u16(header.kind)?;
    Ok(Some(IncomingRequest::Standard(decode_request(kind, &payload)?)))
}

pub async fn write_daemon_management_request<W>(
    writer: &mut W,
    request: &DaemonManagementRequest,
) -> Result<()>
where
    W: AsyncWrite + Unpin,
{
    let (kind, payload) = encode_daemon_management_request(request)?;
    write_frame(writer, DAEMON_MANAGEMENT_VERSION, kind, &payload).await
}

pub async fn read_daemon_management_response<R>(
    reader: &mut R,
) -> Result<Option<DaemonManagementResponse>>
where
    R: AsyncRead + Unpin,
{
    let Some((header, payload)) = read_raw_frame(reader).await? else {
        return Ok(None);
    };
    ensure!(
        header.version == DAEMON_MANAGEMENT_VERSION,
        "unsupported daemon management protocol version `{}`",
        header.version
    );
    decode_daemon_management_response(header.kind, &payload).map(Some)
}

pub async fn write_daemon_management_response<W>(
    writer: &mut W,
    response: &DaemonManagementResponse,
) -> Result<()>
where
    W: AsyncWrite + Unpin,
{
    let (kind, payload) = encode_daemon_management_response(response)?;
    write_frame(writer, DAEMON_MANAGEMENT_VERSION, kind, &payload).await
}

async fn write_frame<W>(writer: &mut W, version: u16, kind: u16, payload: &[u8]) -> Result<()>
where
    W: AsyncWrite + Unpin,
{
    ensure!(
        payload.len() <= MAX_FRAME_PAYLOAD,
        "frame payload too large: {} bytes (limit {MAX_FRAME_PAYLOAD})",
        payload.len()
    );

    let mut header = [0_u8; FRAME_HEADER_LEN];
    header[0..4].copy_from_slice(&FRAME_MAGIC.to_le_bytes());
    header[4..6].copy_from_slice(&version.to_le_bytes());
    header[6..8].copy_from_slice(&kind.to_le_bytes());
    header[8..10].copy_from_slice(&0_u16.to_le_bytes());
    header[10..12].copy_from_slice(&0_u16.to_le_bytes());
    header[12..16].copy_from_slice(&(payload.len() as u32).to_le_bytes());

    writer.write_all(&header).await?;
    writer.write_all(payload).await?;
    writer.flush().await?;
    Ok(())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct FrameHeader {
    version: u16,
    kind: u16,
}

async fn read_standard_frame<R>(reader: &mut R) -> Result<Option<(MessageKind, Vec<u8>)>>
where
    R: AsyncRead + Unpin,
{
    let Some((header, payload)) = read_raw_frame(reader).await? else {
        return Ok(None);
    };

    ensure!(
        header.version == PROTOCOL_VERSION,
        "unsupported protocol version `{}`",
        header.version
    );
    let kind = MessageKind::from_u16(header.kind)?;
    Ok(Some((kind, payload)))
}

async fn read_raw_frame<R>(reader: &mut R) -> Result<Option<(FrameHeader, Vec<u8>)>>
where
    R: AsyncRead + Unpin,
{
    let mut header = [0_u8; FRAME_HEADER_LEN];
    let bytes_read = reader.read(&mut header[..1]).await?;
    if bytes_read == 0 {
        return Ok(None);
    }
    if let Err(err) = reader.read_exact(&mut header[1..]).await {
        if err.kind() == std::io::ErrorKind::UnexpectedEof {
            bail!("truncated frame header");
        }
        return Err(err.into());
    }

    let magic = u32::from_le_bytes(header[0..4].try_into().unwrap());
    ensure!(magic == FRAME_MAGIC, "invalid frame magic");

    let version = u16::from_le_bytes(header[4..6].try_into().unwrap());
    let kind = u16::from_le_bytes(header[6..8].try_into().unwrap());
    let _flags = u16::from_le_bytes(header[8..10].try_into().unwrap());
    let _reserved = u16::from_le_bytes(header[10..12].try_into().unwrap());
    let payload_len = u32::from_le_bytes(header[12..16].try_into().unwrap()) as usize;
    ensure!(
        payload_len <= MAX_FRAME_PAYLOAD,
        "frame payload too large: {payload_len} bytes (limit {MAX_FRAME_PAYLOAD})"
    );

    let mut payload = vec![0_u8; payload_len];
    reader.read_exact(&mut payload).await?;
    Ok(Some((FrameHeader { version, kind }, payload)))
}

fn encode_request(request: &Request) -> Result<(MessageKind, Vec<u8>)> {
    let mut payload = Vec::new();
    let kind = match request {
        Request::GetDaemonInfo => MessageKind::GetDaemonInfoRequest,
        Request::ShutdownDaemon => MessageKind::ShutdownDaemonRequest,
        Request::CreateSession { cwd, name, agent, model } => {
            put_string(&mut payload, cwd)?;
            put_optional_string(&mut payload, name.as_deref())?;
            put_string(&mut payload, agent)?;
            put_optional_string(&mut payload, model.as_deref())?;
            MessageKind::CreateSessionRequest
        }
        Request::KillSession { session_id, remove, force } => {
            put_string(&mut payload, session_id)?;
            put_bool(&mut payload, *remove);
            put_bool(&mut payload, *force);
            MessageKind::KillSessionRequest
        }
        Request::ResolveSessionRuntime { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::ResolveSessionRuntimeRequest
        }
        Request::AttachSession { session_id, kind, cols, rows, pixel_width, pixel_height } => {
            put_string(&mut payload, session_id)?;
            put_attachment_kind(&mut payload, *kind);
            put_u16(&mut payload, *cols);
            put_u16(&mut payload, *rows);
            put_u16(&mut payload, *pixel_width);
            put_u16(&mut payload, *pixel_height);
            MessageKind::AttachSessionRequest
        }
        Request::AttachResize { cols, rows, pixel_width, pixel_height } => {
            put_u16(&mut payload, *cols);
            put_u16(&mut payload, *rows);
            put_u16(&mut payload, *pixel_width);
            put_u16(&mut payload, *pixel_height);
            MessageKind::AttachResizeRequest
        }
        Request::DetachSession { session_id, all } => {
            put_string(&mut payload, session_id)?;
            put_bool(&mut payload, *all);
            MessageKind::DetachSessionRequest
        }
        Request::DetachAttachment { session_id, attach_id } => {
            put_string(&mut payload, session_id)?;
            put_string(&mut payload, attach_id)?;
            MessageKind::DetachAttachmentRequest
        }
        Request::AttachInput { data } => {
            put_bytes(&mut payload, data)?;
            MessageKind::AttachInputRequest
        }
        Request::AttachSnapshot => MessageKind::AttachSnapshotRequest,
        Request::SendInput { session_id, data, source_session_id } => {
            put_string(&mut payload, session_id)?;
            put_bytes(&mut payload, data)?;
            put_optional_string(&mut payload, source_session_id.as_deref())?;
            MessageKind::SendInputRequest
        }
        Request::SwitchAttachedSession { source_session_id, target_session_id } => {
            put_string(&mut payload, source_session_id)?;
            put_string(&mut payload, target_session_id)?;
            MessageKind::SwitchAttachedSessionRequest
        }
        Request::GetSession { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::GetSessionRequest
        }
        Request::ListSessions => MessageKind::ListSessionsRequest,
        Request::ListAttachments { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::ListAttachmentsRequest
        }
        Request::GetHistory { session_id, vt } => {
            put_string(&mut payload, session_id)?;
            put_bool(&mut payload, *vt);
            MessageKind::GetHistoryRequest
        }
    };
    Ok((kind, payload))
}

fn decode_request(kind: MessageKind, payload: &[u8]) -> Result<Request> {
    let mut cursor = Cursor::new(payload);
    let request = match kind {
        MessageKind::GetDaemonInfoRequest => Request::GetDaemonInfo,
        MessageKind::ShutdownDaemonRequest => Request::ShutdownDaemon,
        MessageKind::CreateSessionRequest => Request::CreateSession {
            cwd: cursor.take_string()?,
            name: cursor.take_optional_string()?,
            agent: cursor.take_string()?,
            model: cursor.take_optional_string()?,
        },
        MessageKind::KillSessionRequest => Request::KillSession {
            session_id: cursor.take_string()?,
            remove: cursor.take_bool()?,
            force: cursor.take_bool()?,
        },
        MessageKind::ResolveSessionRuntimeRequest => {
            Request::ResolveSessionRuntime { session_id: cursor.take_string()? }
        }
        MessageKind::AttachSessionRequest => Request::AttachSession {
            session_id: cursor.take_string()?,
            kind: cursor.take_attachment_kind()?,
            cols: cursor.take_u16()?,
            rows: cursor.take_u16()?,
            pixel_width: cursor.take_u16()?,
            pixel_height: cursor.take_u16()?,
        },
        MessageKind::AttachResizeRequest => Request::AttachResize {
            cols: cursor.take_u16()?,
            rows: cursor.take_u16()?,
            pixel_width: cursor.take_u16()?,
            pixel_height: cursor.take_u16()?,
        },
        MessageKind::DetachSessionRequest => {
            Request::DetachSession { session_id: cursor.take_string()?, all: cursor.take_bool()? }
        }
        MessageKind::DetachAttachmentRequest => Request::DetachAttachment {
            session_id: cursor.take_string()?,
            attach_id: cursor.take_string()?,
        },
        MessageKind::AttachInputRequest => Request::AttachInput { data: cursor.take_bytes()? },
        MessageKind::AttachSnapshotRequest => Request::AttachSnapshot,
        MessageKind::SendInputRequest => Request::SendInput {
            session_id: cursor.take_string()?,
            data: cursor.take_bytes()?,
            source_session_id: cursor.take_optional_string()?,
        },
        MessageKind::SwitchAttachedSessionRequest => Request::SwitchAttachedSession {
            source_session_id: cursor.take_string()?,
            target_session_id: cursor.take_string()?,
        },
        MessageKind::GetSessionRequest => Request::GetSession { session_id: cursor.take_string()? },
        MessageKind::ListSessionsRequest => Request::ListSessions,
        MessageKind::ListAttachmentsRequest => {
            Request::ListAttachments { session_id: cursor.take_string()? }
        }
        MessageKind::GetHistoryRequest => {
            Request::GetHistory { session_id: cursor.take_string()?, vt: cursor.take_bool()? }
        }
        other => bail!("unexpected response message kind `{other:?}` while decoding request"),
    };
    cursor.finish()?;
    Ok(request)
}

fn encode_response(response: &Response) -> Result<(MessageKind, Vec<u8>)> {
    let mut payload = Vec::new();
    let kind = match response {
        Response::DaemonInfo { info } => {
            put_daemon_info(&mut payload, info)?;
            MessageKind::DaemonInfoResponse
        }
        Response::CreateSession { session } => {
            put_create_session_result(&mut payload, session)?;
            MessageKind::CreateSessionResponse
        }
        Response::KillSession { removed, was_running } => {
            put_bool(&mut payload, *removed);
            put_bool(&mut payload, *was_running);
            MessageKind::KillSessionResponse
        }
        Response::RuntimeEndpoint { socket_path } => {
            put_string(&mut payload, socket_path)?;
            MessageKind::RuntimeEndpointResponse
        }
        Response::Attached { attach_id, snapshot } => {
            put_string(&mut payload, attach_id)?;
            put_bytes(&mut payload, snapshot)?;
            MessageKind::AttachedResponse
        }
        Response::AttachSnapshot { snapshot } => {
            put_bytes(&mut payload, snapshot)?;
            MessageKind::AttachSnapshotResponse
        }
        Response::SessionEnded { session_id, status, exit_code, error } => {
            put_string(&mut payload, session_id)?;
            put_session_status(&mut payload, *status);
            put_optional_i32(&mut payload, *exit_code);
            put_optional_string(&mut payload, error.as_deref())?;
            MessageKind::SessionEndedResponse
        }
        Response::InputAccepted => MessageKind::InputAcceptedResponse,
        Response::Session { session } => {
            put_session_record(&mut payload, session)?;
            MessageKind::SessionResponse
        }
        Response::Sessions { sessions } => {
            put_len(&mut payload, sessions.len())?;
            for session in sessions {
                put_session_record(&mut payload, session)?;
            }
            MessageKind::SessionsResponse
        }
        Response::Attachments { attachments } => {
            put_len(&mut payload, attachments.len())?;
            for attachment in attachments {
                put_attachment_record(&mut payload, attachment)?;
            }
            MessageKind::AttachmentsResponse
        }
        Response::History { data } => {
            put_string(&mut payload, data)?;
            MessageKind::HistoryResponse
        }
        Response::PtyOutput { data } => {
            put_bytes(&mut payload, data)?;
            MessageKind::PtyOutputResponse
        }
        Response::SwitchSession { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::SwitchSessionResponse
        }
        Response::EndOfStream => MessageKind::EndOfStreamResponse,
        Response::Error { message } => {
            put_string(&mut payload, message)?;
            MessageKind::ErrorResponse
        }
        Response::Ok => MessageKind::OkResponse,
    };
    Ok((kind, payload))
}

fn decode_response(kind: MessageKind, payload: &[u8]) -> Result<Response> {
    let mut cursor = Cursor::new(payload);
    let response = match kind {
        MessageKind::DaemonInfoResponse => {
            Response::DaemonInfo { info: cursor.take_daemon_info()? }
        }
        MessageKind::CreateSessionResponse => {
            Response::CreateSession { session: cursor.take_create_session_result()? }
        }
        MessageKind::KillSessionResponse => {
            Response::KillSession { removed: cursor.take_bool()?, was_running: cursor.take_bool()? }
        }
        MessageKind::RuntimeEndpointResponse => {
            Response::RuntimeEndpoint { socket_path: cursor.take_string()? }
        }
        MessageKind::AttachedResponse => {
            Response::Attached { attach_id: cursor.take_string()?, snapshot: cursor.take_bytes()? }
        }
        MessageKind::AttachSnapshotResponse => {
            Response::AttachSnapshot { snapshot: cursor.take_bytes()? }
        }
        MessageKind::SessionEndedResponse => Response::SessionEnded {
            session_id: cursor.take_string()?,
            status: cursor.take_session_status()?,
            exit_code: cursor.take_optional_i32()?,
            error: cursor.take_optional_string()?,
        },
        MessageKind::InputAcceptedResponse => Response::InputAccepted,
        MessageKind::SessionResponse => {
            Response::Session { session: cursor.take_session_record()? }
        }
        MessageKind::SessionsResponse => {
            let len = cursor.take_len()?;
            let mut sessions = Vec::with_capacity(len);
            for _ in 0..len {
                sessions.push(cursor.take_session_record()?);
            }
            Response::Sessions { sessions }
        }
        MessageKind::AttachmentsResponse => {
            let len = cursor.take_len()?;
            let mut attachments = Vec::with_capacity(len);
            for _ in 0..len {
                attachments.push(cursor.take_attachment_record()?);
            }
            Response::Attachments { attachments }
        }
        MessageKind::HistoryResponse => Response::History { data: cursor.take_string()? },
        MessageKind::PtyOutputResponse => Response::PtyOutput { data: cursor.take_bytes()? },
        MessageKind::SwitchSessionResponse => {
            Response::SwitchSession { session_id: cursor.take_string()? }
        }
        MessageKind::EndOfStreamResponse => Response::EndOfStream,
        MessageKind::ErrorResponse => Response::Error { message: cursor.take_string()? },
        MessageKind::OkResponse => Response::Ok,
        other => bail!("unexpected request message kind `{other:?}` while decoding response"),
    };
    cursor.finish()?;
    Ok(response)
}

fn encode_daemon_management_request(request: &DaemonManagementRequest) -> Result<(u16, Vec<u8>)> {
    let (kind, payload) = match request {
        DaemonManagementRequest::Status => (DAEMON_STATUS_REQUEST_KIND, Vec::new()),
        DaemonManagementRequest::Shutdown { force } => (
            DAEMON_SHUTDOWN_REQUEST_KIND,
            serde_json::to_vec(&serde_json::json!({ "force": force }))?,
        ),
    };
    Ok((kind, payload))
}

fn decode_daemon_management_request(kind: u16, payload: &[u8]) -> Result<DaemonManagementRequest> {
    match kind {
        DAEMON_STATUS_REQUEST_KIND => {
            ensure!(payload.is_empty(), "unexpected status request payload");
            Ok(DaemonManagementRequest::Status)
        }
        DAEMON_SHUTDOWN_REQUEST_KIND => {
            #[derive(Deserialize)]
            struct ShutdownPayload {
                force: bool,
            }

            let payload: ShutdownPayload = serde_json::from_slice(payload)?;
            Ok(DaemonManagementRequest::Shutdown { force: payload.force })
        }
        other => bail!("unknown daemon management request kind `{other}`"),
    }
}

fn encode_daemon_management_response(
    response: &DaemonManagementResponse,
) -> Result<(u16, Vec<u8>)> {
    let (kind, payload) = match response {
        DaemonManagementResponse::Status { status } => {
            (DAEMON_STATUS_RESPONSE_KIND, serde_json::to_vec(status)?)
        }
        DaemonManagementResponse::Shutdown { stopped, running_sessions, message } => (
            DAEMON_SHUTDOWN_RESPONSE_KIND,
            serde_json::to_vec(&serde_json::json!({
                "stopped": stopped,
                "running_sessions": running_sessions,
                "message": message,
            }))?,
        ),
        DaemonManagementResponse::Error { message } => (
            DAEMON_ERROR_RESPONSE_KIND,
            serde_json::to_vec(&serde_json::json!({ "message": message }))?,
        ),
    };
    Ok((kind, payload))
}

fn decode_daemon_management_response(
    kind: u16,
    payload: &[u8],
) -> Result<DaemonManagementResponse> {
    match kind {
        DAEMON_STATUS_RESPONSE_KIND => {
            Ok(DaemonManagementResponse::Status { status: serde_json::from_slice(payload)? })
        }
        DAEMON_SHUTDOWN_RESPONSE_KIND => {
            #[derive(Deserialize)]
            struct ShutdownPayload {
                stopped: bool,
                running_sessions: bool,
                message: String,
            }

            let payload: ShutdownPayload = serde_json::from_slice(payload)?;
            Ok(DaemonManagementResponse::Shutdown {
                stopped: payload.stopped,
                running_sessions: payload.running_sessions,
                message: payload.message,
            })
        }
        DAEMON_ERROR_RESPONSE_KIND => {
            #[derive(Deserialize)]
            struct ErrorPayload {
                message: String,
            }

            let payload: ErrorPayload = serde_json::from_slice(payload)?;
            Ok(DaemonManagementResponse::Error { message: payload.message })
        }
        other => bail!("unknown daemon management response kind `{other}`"),
    }
}

fn put_daemon_info(buf: &mut Vec<u8>, info: &DaemonInfo) -> Result<()> {
    put_string(buf, &info.daemon_version)?;
    buf.extend_from_slice(&info.protocol_version.to_le_bytes());
    Ok(())
}

fn put_session_status(buf: &mut Vec<u8>, status: SessionStatus) {
    buf.push(match status {
        SessionStatus::Creating => 1,
        SessionStatus::Running => 2,
        SessionStatus::Exited => 5,
        SessionStatus::Failed => 6,
        SessionStatus::UnknownRecovered => 7,
    });
}

fn put_attachment_kind(buf: &mut Vec<u8>, kind: AttachmentKind) {
    buf.push(match kind {
        AttachmentKind::Attach => 1,
        AttachmentKind::Tui => 2,
    });
}

fn put_attention_level(buf: &mut Vec<u8>, attention: AttentionLevel) {
    buf.push(match attention {
        AttentionLevel::Info => 1,
        AttentionLevel::Notice => 2,
        AttentionLevel::Action => 3,
    });
}

fn put_session_mode(buf: &mut Vec<u8>, mode: SessionMode) {
    buf.push(match mode {
        SessionMode::Execute => 1,
        SessionMode::Plan => 2,
    });
}

fn put_create_session_result(buf: &mut Vec<u8>, session: &CreateSessionResult) -> Result<()> {
    put_string(buf, &session.session_id)?;
    put_string(buf, &session.cwd)?;
    put_session_status(buf, session.status);
    put_session_mode(buf, session.mode);
    Ok(())
}

fn put_session_record(buf: &mut Vec<u8>, session: &SessionRecord) -> Result<()> {
    put_string(buf, &session.session_id)?;
    put_string(buf, &session.agent)?;
    put_optional_string(buf, session.model.as_deref())?;
    put_session_mode(buf, session.mode);
    put_string(buf, &session.cwd)?;
    put_session_status(buf, session.status);
    put_optional_u32(buf, session.worker_pid);
    put_optional_u32(buf, session.agent_pid);
    put_optional_i32(buf, session.exit_code);
    put_optional_string(buf, session.error.as_deref())?;
    put_attention_level(buf, session.attention);
    put_optional_string(buf, session.attention_summary.as_deref())?;
    put_datetime(buf, &session.created_at);
    put_datetime(buf, &session.updated_at);
    put_optional_datetime(buf, session.exited_at.as_ref());
    Ok(())
}

fn put_attachment_record(buf: &mut Vec<u8>, attachment: &AttachmentRecord) -> Result<()> {
    put_string(buf, &attachment.attach_id)?;
    put_string(buf, &attachment.session_id)?;
    put_attachment_kind(buf, attachment.kind);
    put_datetime(buf, &attachment.connected_at);
    Ok(())
}

fn put_datetime(buf: &mut Vec<u8>, value: &DateTime<Utc>) {
    buf.extend_from_slice(&value.timestamp().to_le_bytes());
    buf.extend_from_slice(&value.timestamp_subsec_nanos().to_le_bytes());
}

fn put_optional_datetime(buf: &mut Vec<u8>, value: Option<&DateTime<Utc>>) {
    match value {
        Some(value) => {
            buf.push(1);
            put_datetime(buf, value);
        }
        None => buf.push(0),
    }
}

fn put_optional_u32(buf: &mut Vec<u8>, value: Option<u32>) {
    match value {
        Some(value) => {
            buf.push(1);
            put_u32(buf, value);
        }
        None => buf.push(0),
    }
}

fn put_u32(buf: &mut Vec<u8>, value: u32) {
    buf.extend_from_slice(&value.to_le_bytes());
}

fn put_optional_i32(buf: &mut Vec<u8>, value: Option<i32>) {
    match value {
        Some(value) => {
            buf.push(1);
            buf.extend_from_slice(&value.to_le_bytes());
        }
        None => buf.push(0),
    }
}

fn put_optional_string(buf: &mut Vec<u8>, value: Option<&str>) -> Result<()> {
    match value {
        Some(value) => {
            buf.push(1);
            put_string(buf, value)?;
        }
        None => buf.push(0),
    }
    Ok(())
}

fn put_bool(buf: &mut Vec<u8>, value: bool) {
    buf.push(u8::from(value));
}

fn put_u16(buf: &mut Vec<u8>, value: u16) {
    buf.extend_from_slice(&value.to_le_bytes());
}

fn put_string(buf: &mut Vec<u8>, value: &str) -> Result<()> {
    put_bytes(buf, value.as_bytes())
}

fn put_bytes(buf: &mut Vec<u8>, value: &[u8]) -> Result<()> {
    put_len(buf, value.len())?;
    buf.extend_from_slice(value);
    Ok(())
}

fn put_len(buf: &mut Vec<u8>, len: usize) -> Result<()> {
    let len = u32::try_from(len).context("length exceeds u32")?;
    buf.extend_from_slice(&len.to_le_bytes());
    Ok(())
}

struct Cursor<'a> {
    payload: &'a [u8],
    position: usize,
}

impl<'a> Cursor<'a> {
    fn new(payload: &'a [u8]) -> Self {
        Self { payload, position: 0 }
    }

    fn finish(&self) -> Result<()> {
        ensure!(self.position == self.payload.len(), "unexpected trailing payload bytes");
        Ok(())
    }

    fn take_bytes(&mut self) -> Result<Vec<u8>> {
        let len = self.take_u32()? as usize;
        let bytes = self.take_exact(len)?;
        Ok(bytes.to_vec())
    }

    fn take_string(&mut self) -> Result<String> {
        String::from_utf8(self.take_bytes()?).map_err(Into::into)
    }

    fn take_bool(&mut self) -> Result<bool> {
        Ok(match self.take_u8()? {
            0 => false,
            1 => true,
            other => bail!("invalid bool value `{other}`"),
        })
    }

    fn take_optional_string(&mut self) -> Result<Option<String>> {
        Ok(if self.take_bool()? { Some(self.take_string()?) } else { None })
    }

    fn take_optional_u32(&mut self) -> Result<Option<u32>> {
        Ok(if self.take_bool()? { Some(self.take_u32()?) } else { None })
    }

    fn take_optional_i32(&mut self) -> Result<Option<i32>> {
        Ok(if self.take_bool()? { Some(self.take_i32()?) } else { None })
    }

    fn take_len(&mut self) -> Result<usize> {
        Ok(self.take_u32()? as usize)
    }

    fn take_session_status(&mut self) -> Result<SessionStatus> {
        Ok(match self.take_u8()? {
            1 => SessionStatus::Creating,
            2 => SessionStatus::Running,
            3 => SessionStatus::UnknownRecovered,
            5 => SessionStatus::Exited,
            6 => SessionStatus::Failed,
            7 => SessionStatus::UnknownRecovered,
            other => bail!("invalid session status `{other}`"),
        })
    }

    fn take_attachment_kind(&mut self) -> Result<AttachmentKind> {
        Ok(match self.take_u8()? {
            1 => AttachmentKind::Attach,
            2 => AttachmentKind::Tui,
            other => bail!("invalid attachment kind `{other}`"),
        })
    }

    fn take_attention_level(&mut self) -> Result<AttentionLevel> {
        Ok(match self.take_u8()? {
            1 => AttentionLevel::Info,
            2 => AttentionLevel::Notice,
            3 => AttentionLevel::Action,
            other => bail!("invalid attention level `{other}`"),
        })
    }

    fn take_session_mode(&mut self) -> Result<SessionMode> {
        Ok(match self.take_u8()? {
            1 => SessionMode::Execute,
            2 => SessionMode::Plan,
            other => bail!("invalid session mode `{other}`"),
        })
    }

    fn take_datetime(&mut self) -> Result<DateTime<Utc>> {
        let seconds = self.take_i64()?;
        let nanos = self.take_u32()?;
        Utc.timestamp_opt(seconds, nanos).single().ok_or_else(|| anyhow!("invalid UTC timestamp"))
    }

    fn take_optional_datetime(&mut self) -> Result<Option<DateTime<Utc>>> {
        Ok(if self.take_bool()? { Some(self.take_datetime()?) } else { None })
    }

    fn take_daemon_info(&mut self) -> Result<DaemonInfo> {
        Ok(DaemonInfo { daemon_version: self.take_string()?, protocol_version: self.take_u16()? })
    }

    fn take_create_session_result(&mut self) -> Result<CreateSessionResult> {
        Ok(CreateSessionResult {
            session_id: self.take_string()?,
            cwd: self.take_string()?,
            status: self.take_session_status()?,
            mode: self.take_session_mode()?,
        })
    }

    fn take_session_record(&mut self) -> Result<SessionRecord> {
        Ok(SessionRecord {
            session_id: self.take_string()?,
            agent: self.take_string()?,
            model: self.take_optional_string()?,
            mode: self.take_session_mode()?,
            cwd: self.take_string()?,
            status: self.take_session_status()?,
            worker_pid: self.take_optional_u32()?,
            agent_pid: self.take_optional_u32()?,
            exit_code: self.take_optional_i32()?,
            error: self.take_optional_string()?,
            attention: self.take_attention_level()?,
            attention_summary: self.take_optional_string()?,
            created_at: self.take_datetime()?,
            updated_at: self.take_datetime()?,
            exited_at: self.take_optional_datetime()?,
        })
    }

    fn take_attachment_record(&mut self) -> Result<AttachmentRecord> {
        Ok(AttachmentRecord {
            attach_id: self.take_string()?,
            session_id: self.take_string()?,
            kind: self.take_attachment_kind()?,
            connected_at: self.take_datetime()?,
        })
    }

    fn take_u8(&mut self) -> Result<u8> {
        Ok(self.take_exact(1)?[0])
    }

    fn take_u16(&mut self) -> Result<u16> {
        Ok(u16::from_le_bytes(self.take_exact(2)?.try_into().unwrap()))
    }

    fn take_u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.take_exact(4)?.try_into().unwrap()))
    }

    fn take_i32(&mut self) -> Result<i32> {
        Ok(i32::from_le_bytes(self.take_exact(4)?.try_into().unwrap()))
    }

    fn take_i64(&mut self) -> Result<i64> {
        Ok(i64::from_le_bytes(self.take_exact(8)?.try_into().unwrap()))
    }

    fn take_exact(&mut self, len: usize) -> Result<&'a [u8]> {
        let end =
            self.position.checked_add(len).ok_or_else(|| anyhow!("payload length overflow"))?;
        ensure!(end <= self.payload.len(), "truncated payload");
        let bytes = &self.payload[self.position..end];
        self.position = end;
        Ok(bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::{
        Cursor, DAEMON_MANAGEMENT_VERSION, DaemonInfo, DaemonManagementRequest,
        DaemonManagementResponse, DaemonManagementStatus, IncomingRequest, MAX_FRAME_PAYLOAD,
        PROTOCOL_VERSION, Request, Response, decode_request, decode_response, encode_request,
        encode_response, read_incoming_request, read_request, write_daemon_management_request,
        write_daemon_management_response,
    };
    use crate::session::{
        AttachmentKind, AttachmentRecord, AttentionLevel, CreateSessionResult, SessionMode,
        SessionRecord, SessionStatus,
    };
    use chrono::{TimeZone, Utc};
    use tokio::io::AsyncWriteExt;

    #[test]
    fn request_round_trips_binary_payloads() {
        let request = Request::SendInput {
            session_id: "demo".to_string(),
            data: vec![0, 1, 2, 255],
            source_session_id: Some("origin".to_string()),
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn attach_resize_round_trips() {
        let request =
            Request::AttachResize { cols: 120, rows: 48, pixel_width: 960, pixel_height: 768 };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn attach_snapshot_request_round_trips() {
        let request = Request::AttachSnapshot;
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn switch_attached_session_round_trips() {
        let request = Request::SwitchAttachedSession {
            source_session_id: "source".to_string(),
            target_session_id: "target".to_string(),
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn detach_session_round_trips() {
        let request = Request::DetachSession { session_id: "demo".to_string(), all: true };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn attach_session_round_trips_kind() {
        let request = Request::AttachSession {
            session_id: "demo".to_string(),
            kind: AttachmentKind::Tui,
            cols: 120,
            rows: 48,
            pixel_width: 960,
            pixel_height: 768,
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn detach_attachment_round_trips() {
        let request = Request::DetachAttachment {
            session_id: "demo".to_string(),
            attach_id: "attach-1".to_string(),
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn list_attachments_round_trips() {
        let request = Request::ListAttachments { session_id: "demo".to_string() };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn get_history_round_trips() {
        let request = Request::GetHistory { session_id: "demo".to_string(), vt: true };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn legacy_paused_status_decodes_as_unknown_recovered() {
        let mut cursor = Cursor::new(&[3]);
        assert_eq!(cursor.take_session_status().unwrap(), SessionStatus::UnknownRecovered);
    }

    #[test]
    fn create_session_round_trips_cwd() {
        let request = Request::CreateSession {
            cwd: "/tmp/x".to_string(),
            name: Some("fix".to_string()),
            agent: "codex".to_string(),
            model: Some("m".to_string()),
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn kill_session_round_trips_with_force() {
        let request =
            Request::KillSession { session_id: "demo".to_string(), remove: true, force: true };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn response_round_trips_snapshot_bytes() {
        let response =
            Response::Attached { attach_id: "attach-1".to_string(), snapshot: vec![0, 1, 2, 3, 4] };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn attach_snapshot_response_round_trips() {
        let response = Response::AttachSnapshot { snapshot: vec![4, 3, 2, 1] };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn switch_session_response_round_trips() {
        let response = Response::SwitchSession { session_id: "target".to_string() };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn history_response_round_trips() {
        let response = Response::History { data: "\u{1b}[31mhello\u{1b}[0m".to_string() };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn sessions_response_round_trips() {
        let now = Utc::now();
        let response = Response::Sessions {
            sessions: vec![SessionRecord {
                session_id: "demo".to_string(),
                agent: "codex".to_string(),
                model: Some("gpt-5.4".to_string()),
                mode: SessionMode::Execute,
                cwd: "/tmp/demo".to_string(),
                status: SessionStatus::Running,
                worker_pid: Some(123),
                agent_pid: Some(456),
                exit_code: None,
                error: None,
                attention: AttentionLevel::Info,
                attention_summary: Some("fix".to_string()),
                created_at: now,
                updated_at: now,
                exited_at: None,
            }],
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn attachments_response_round_trips() {
        let now = Utc::now();
        let response = Response::Attachments {
            attachments: vec![AttachmentRecord {
                attach_id: "attach-1".to_string(),
                session_id: "demo".to_string(),
                kind: AttachmentKind::Attach,
                connected_at: now,
            }],
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn daemon_info_carries_protocol_version() {
        let response = Response::DaemonInfo {
            info: DaemonInfo {
                daemon_version: "1.2.3".to_string(),
                protocol_version: PROTOCOL_VERSION,
            },
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn session_ended_response_round_trips() {
        let response = Response::SessionEnded {
            session_id: "demo".to_string(),
            status: SessionStatus::Exited,
            exit_code: Some(0),
            error: None,
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[tokio::test]
    async fn daemon_management_status_round_trips() {
        let (mut writer, mut reader) = tokio::io::duplex(1024);
        let request = DaemonManagementRequest::Shutdown { force: true };
        write_daemon_management_request(&mut writer, &request).await.unwrap();
        drop(writer);

        let incoming = read_incoming_request(&mut reader).await.unwrap().unwrap();
        assert_eq!(incoming, IncomingRequest::DaemonManagement(request));
    }

    #[tokio::test]
    async fn daemon_management_response_round_trips() {
        let (mut writer, mut reader) = tokio::io::duplex(1024);
        let response = DaemonManagementResponse::Status {
            status: DaemonManagementStatus {
                daemon_version: "1.2.3".to_string(),
                protocol_version: PROTOCOL_VERSION,
                pid: 42,
                root: "/tmp/agentd".to_string(),
                socket: "/tmp/agentd/agentd.sock".to_string(),
                running_sessions: false,
            },
        };
        write_daemon_management_response(&mut writer, &response).await.unwrap();
        drop(writer);

        let decoded = super::read_daemon_management_response(&mut reader).await.unwrap().unwrap();
        assert_eq!(decoded, response);
    }

    #[tokio::test]
    async fn incoming_request_accepts_daemon_management_version() {
        let (mut writer, mut reader) = tokio::io::duplex(1024);
        writer.write_all(&super::FRAME_MAGIC.to_le_bytes()).await.unwrap();
        writer.write_all(&DAEMON_MANAGEMENT_VERSION.to_le_bytes()).await.unwrap();
        writer.write_all(&super::DAEMON_STATUS_REQUEST_KIND.to_le_bytes()).await.unwrap();
        writer.write_all(&0_u16.to_le_bytes()).await.unwrap();
        writer.write_all(&0_u16.to_le_bytes()).await.unwrap();
        writer.write_all(&0_u32.to_le_bytes()).await.unwrap();
        drop(writer);

        let incoming = read_incoming_request(&mut reader).await.unwrap().unwrap();
        assert_eq!(incoming, IncomingRequest::DaemonManagement(DaemonManagementRequest::Status));
    }

    #[test]
    fn create_session_result_round_trips() {
        let response = Response::CreateSession {
            session: CreateSessionResult {
                session_id: "demo".to_string(),
                cwd: "/tmp/demo".to_string(),
                status: SessionStatus::Running,
                mode: SessionMode::Execute,
            },
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[tokio::test]
    async fn truncated_frame_header_returns_error() {
        let (mut writer, mut reader) = tokio::io::duplex(16);
        writer.write_all(&[1, 2, 3]).await.unwrap();
        drop(writer);

        let err = read_request(&mut reader).await.unwrap_err();
        assert!(err.to_string().contains("truncated frame header"));
    }

    // The golden frames below are the exact bytes go/internal/protocol produces
    // for the same values (TestAttachSessionGoldenFrame in protocol_test.go, and
    // the other frames dumped from WriteRequest/WriteResponse). If either side
    // changes an encoding, one of these tests or the Go tests fails.

    async fn request_frame(request: &Request) -> Vec<u8> {
        let mut buf = Vec::new();
        super::write_request(&mut buf, request).await.unwrap();
        buf
    }

    async fn response_frame(response: &Response) -> Vec<u8> {
        let mut buf = Vec::new();
        super::write_response(&mut buf, response).await.unwrap();
        buf
    }

    async fn assert_request_golden(request: Request, golden: &[u8]) {
        assert_eq!(request_frame(&request).await, golden);
        let mut reader = golden;
        assert_eq!(read_request(&mut reader).await.unwrap().unwrap(), request);
    }

    async fn assert_response_golden(response: Response, golden: &[u8]) {
        assert_eq!(response_frame(&response).await, golden);
        let mut reader = golden;
        assert_eq!(super::read_response(&mut reader).await.unwrap().unwrap(), response);
    }

    #[tokio::test]
    async fn attach_session_matches_go_golden_frame() {
        let golden = [
            0x50, 0x44, 0x47, 0x41, // magic "AGDP" little-endian
            33, 0, // protocol version
            7, 0, // AttachSessionRequest
            0, 0, 0, 0, // flags, reserved
            15, 0, 0, 0, // payload length
            2, 0, 0, 0, b'a', b'b', // session id
            2,    // AttachmentKind::Tui
            0x02, 0x01, 0x04, 0x03, // cols, rows
            0x06, 0x05, 0x08, 0x07, // pixel width, pixel height
        ];
        assert_request_golden(
            Request::AttachSession {
                session_id: "ab".to_string(),
                kind: AttachmentKind::Tui,
                cols: 0x0102,
                rows: 0x0304,
                pixel_width: 0x0506,
                pixel_height: 0x0708,
            },
            &golden,
        )
        .await;
    }

    #[tokio::test]
    async fn create_session_matches_go_golden_frame() {
        let golden = [
            0x50, 0x44, 0x47, 0x41, 0x21, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, //
            0x18, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x2f, 0x77, 0x01, 0x03, //
            0x00, 0x00, 0x00, 0x66, 0x69, 0x78, 0x05, 0x00, 0x00, 0x00, 0x63, 0x6f, //
            0x64, 0x65, 0x78, 0x00,
        ];
        assert_request_golden(
            Request::CreateSession {
                cwd: "/w".to_string(),
                name: Some("fix".to_string()),
                agent: "codex".to_string(),
                model: None,
            },
            &golden,
        )
        .await;
    }

    #[tokio::test]
    async fn create_session_result_matches_go_golden_frame() {
        let golden = [
            0x50, 0x44, 0x47, 0x41, 0x21, 0x00, 0x66, 0x00, 0x00, 0x00, 0x00, 0x00, //
            0x0e, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x02, 0x00, //
            0x00, 0x00, 0x2f, 0x77, 0x02, 0x02,
        ];
        assert_response_golden(
            Response::CreateSession {
                session: CreateSessionResult {
                    session_id: "ab".to_string(),
                    cwd: "/w".to_string(),
                    status: SessionStatus::Running,
                    mode: SessionMode::Plan,
                },
            },
            &golden,
        )
        .await;
    }

    #[tokio::test]
    async fn session_ended_matches_go_golden_frame() {
        let golden = [
            0x50, 0x44, 0x47, 0x41, 0x21, 0x00, 0x75, 0x00, 0x00, 0x00, 0x00, 0x00, //
            0x12, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x05, 0x01, //
            0xff, 0xff, 0xff, 0xff, 0x01, 0x01, 0x00, 0x00, 0x00, 0x65,
        ];
        assert_response_golden(
            Response::SessionEnded {
                session_id: "ab".to_string(),
                status: SessionStatus::Exited,
                exit_code: Some(-1),
                error: Some("e".to_string()),
            },
            &golden,
        )
        .await;
    }

    #[tokio::test]
    async fn session_record_matches_go_golden_frame() {
        let golden = [
            0x50, 0x44, 0x47, 0x41, 0x21, 0x00, 0x6c, 0x00, 0x00, 0x00, 0x00, 0x00, //
            0x52, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x02, 0x00, //
            0x00, 0x00, 0x73, 0x68, 0x01, 0x01, 0x00, 0x00, 0x00, 0x6d, 0x01, 0x02, //
            0x00, 0x00, 0x00, 0x2f, 0x77, 0x05, 0x01, 0x07, 0x00, 0x00, 0x00, 0x00, //
            0x01, 0x03, 0x00, 0x00, 0x00, 0x00, 0x02, 0x01, 0x01, 0x00, 0x00, 0x00, //
            0x73, 0x00, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x15, 0xcd, 0x5b, //
            0x07, 0x00, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x15, 0xcd, 0x5b, //
            0x07, 0x01, 0x64, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, //
            0x00, 0x00,
        ];
        let created = Utc.timestamp_opt(1_700_000_000, 123_456_789).single().unwrap();
        let exited = Utc.timestamp_opt(1_700_000_100, 0).single().unwrap();
        assert_response_golden(
            Response::Session {
                session: SessionRecord {
                    session_id: "ab".to_string(),
                    agent: "sh".to_string(),
                    model: Some("m".to_string()),
                    mode: SessionMode::Execute,
                    cwd: "/w".to_string(),
                    status: SessionStatus::Exited,
                    worker_pid: Some(7),
                    agent_pid: None,
                    exit_code: Some(3),
                    error: None,
                    attention: AttentionLevel::Notice,
                    attention_summary: Some("s".to_string()),
                    created_at: created,
                    updated_at: created,
                    exited_at: Some(exited),
                },
            },
            &golden,
        )
        .await;
    }

    fn raw_frame(version: u16, kind: u16, payload: &[u8]) -> Vec<u8> {
        let mut frame = Vec::new();
        frame.extend_from_slice(&super::FRAME_MAGIC.to_le_bytes());
        frame.extend_from_slice(&version.to_le_bytes());
        frame.extend_from_slice(&kind.to_le_bytes());
        frame.extend_from_slice(&[0, 0, 0, 0]);
        frame.extend_from_slice(&(payload.len() as u32).to_le_bytes());
        frame.extend_from_slice(payload);
        frame
    }

    #[tokio::test]
    async fn kinds_removed_in_v33_are_rejected() {
        let payload = [4, 0, 0, 0, b'd', b'e', b'm', b'o'];
        for kind in [4_u16, 5, 10, 19, 20] {
            let frame = raw_frame(PROTOCOL_VERSION, kind, &payload);
            let err = read_request(&mut frame.as_slice()).await.unwrap_err().to_string();
            assert!(err.contains(&format!("unknown message kind `{kind}`")), "{kind}: {err}");
        }
        for kind in [106_u16, 107] {
            let frame = raw_frame(PROTOCOL_VERSION, kind, &payload);
            let err = super::read_response(&mut frame.as_slice()).await.unwrap_err().to_string();
            assert!(err.contains(&format!("unknown message kind `{kind}`")), "{kind}: {err}");
        }
    }

    #[tokio::test]
    async fn v32_frames_are_rejected_by_version() {
        let frame = raw_frame(32, 7, &[]);
        let err = read_request(&mut frame.as_slice()).await.unwrap_err().to_string();
        assert!(err.contains("unsupported protocol version `32`"), "{err}");
        let frame = raw_frame(32, 101, &[]);
        let err = super::read_response(&mut frame.as_slice()).await.unwrap_err().to_string();
        assert!(err.contains("unsupported protocol version `32`"), "{err}");
    }

    fn frame_header_declaring(version: u16, kind: u16, len: u32) -> Vec<u8> {
        let mut frame = Vec::new();
        frame.extend_from_slice(&super::FRAME_MAGIC.to_le_bytes());
        frame.extend_from_slice(&version.to_le_bytes());
        frame.extend_from_slice(&kind.to_le_bytes());
        frame.extend_from_slice(&[0, 0, 0, 0]);
        frame.extend_from_slice(&len.to_le_bytes());
        frame
    }

    #[tokio::test]
    async fn oversized_frame_length_is_rejected_before_reading_payload() {
        // Only the header is sent and the writer stays open: if the reader
        // tried to allocate and fill the payload it would block, not error.
        for (version, kind) in [(PROTOCOL_VERSION, 7_u16), (DAEMON_MANAGEMENT_VERSION, 20_001)] {
            let (mut writer, mut reader) = tokio::io::duplex(64);
            writer.write_all(&frame_header_declaring(version, kind, u32::MAX)).await.unwrap();
            let err = tokio::time::timeout(
                std::time::Duration::from_secs(2),
                super::read_incoming_request(&mut reader),
            )
            .await
            .expect("oversized frame must be rejected promptly")
            .unwrap_err()
            .to_string();
            assert!(err.contains("too large"), "{err}");
            drop(writer);
        }

        let frame = frame_header_declaring(PROTOCOL_VERSION, 101, (MAX_FRAME_PAYLOAD + 1) as u32);
        let err = super::read_response(&mut frame.as_slice()).await.unwrap_err().to_string();
        assert!(err.contains("too large"), "{err}");
    }

    #[tokio::test]
    async fn oversized_frame_is_not_written() {
        let mut buf = Vec::new();
        let payload = vec![0_u8; MAX_FRAME_PAYLOAD + 1];
        let err = super::write_frame(&mut buf, PROTOCOL_VERSION, 7, &payload)
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("too large"), "{err}");
        assert!(buf.is_empty());
    }
}
