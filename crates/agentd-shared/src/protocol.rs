use anyhow::{Context, Result, anyhow, bail, ensure};
use chrono::{DateTime, TimeZone, Utc};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use crate::{
    event::{NewSessionEvent, SessionEvent},
    session::{CreateSessionResult, SessionDiff, SessionRecord, SessionStatus, WorktreeRecord},
};

pub const PROTOCOL_VERSION: u16 = 10;
pub const DAEMON_MANAGEMENT_VERSION: u16 = 1;

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
    Status {
        status: DaemonManagementStatus,
    },
    Shutdown {
        stopped: bool,
        running_sessions: bool,
        message: String,
    },
    Error {
        message: String,
    },
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
        workspace: String,
        task: String,
        agent: String,
    },
    CreateWorktree {
        session_id: String,
    },
    CleanupWorktree {
        session_id: String,
    },
    KillSession {
        session_id: String,
        remove: bool,
    },
    AttachSession {
        session_id: String,
    },
    DetachSession {
        session_id: String,
    },
    AttachInput {
        data: Vec<u8>,
    },
    SendInput {
        session_id: String,
        data: Vec<u8>,
        source_session_id: Option<String>,
    },
    SwitchAttachedSession {
        source_session_id: String,
        target_session_id: String,
    },
    DiffSession {
        session_id: String,
    },
    GetSession {
        session_id: String,
    },
    ListSessions,
    AppendSessionEvents {
        session_id: String,
        events: Vec<NewSessionEvent>,
    },
    StreamLogs {
        session_id: String,
        follow: bool,
    },
    StreamEvents {
        session_id: String,
        follow: bool,
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
    Attached {
        snapshot: Vec<u8>,
    },
    SessionEnded {
        session_id: String,
        status: SessionStatus,
        exit_code: Option<i32>,
        error: Option<String>,
    },
    InputAccepted,
    Worktree {
        worktree: WorktreeRecord,
    },
    Diff {
        diff: SessionDiff,
    },
    Session {
        session: SessionRecord,
    },
    Sessions {
        sessions: Vec<SessionRecord>,
    },
    Event {
        event: SessionEvent,
    },
    LogChunk {
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

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u16)]
enum MessageKind {
    GetDaemonInfoRequest = 1,
    ShutdownDaemonRequest = 2,
    CreateSessionRequest = 3,
    CreateWorktreeRequest = 4,
    CleanupWorktreeRequest = 5,
    KillSessionRequest = 6,
    AttachSessionRequest = 7,
    AttachInputRequest = 8,
    SendInputRequest = 9,
    DetachSessionRequest = 17,
    SwitchAttachedSessionRequest = 16,
    DiffSessionRequest = 10,
    GetSessionRequest = 11,
    ListSessionsRequest = 12,
    AppendSessionEventsRequest = 13,
    StreamLogsRequest = 14,
    StreamEventsRequest = 15,
    DaemonInfoResponse = 101,
    CreateSessionResponse = 102,
    KillSessionResponse = 103,
    AttachedResponse = 104,
    SessionEndedResponse = 117,
    InputAcceptedResponse = 105,
    WorktreeResponse = 106,
    DiffResponse = 107,
    SessionResponse = 108,
    SessionsResponse = 109,
    EventResponse = 110,
    LogChunkResponse = 111,
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
            4 => Self::CreateWorktreeRequest,
            5 => Self::CleanupWorktreeRequest,
            6 => Self::KillSessionRequest,
            7 => Self::AttachSessionRequest,
            8 => Self::AttachInputRequest,
            9 => Self::SendInputRequest,
            17 => Self::DetachSessionRequest,
            16 => Self::SwitchAttachedSessionRequest,
            10 => Self::DiffSessionRequest,
            11 => Self::GetSessionRequest,
            12 => Self::ListSessionsRequest,
            13 => Self::AppendSessionEventsRequest,
            14 => Self::StreamLogsRequest,
            15 => Self::StreamEventsRequest,
            101 => Self::DaemonInfoResponse,
            102 => Self::CreateSessionResponse,
            103 => Self::KillSessionResponse,
            104 => Self::AttachedResponse,
            117 => Self::SessionEndedResponse,
            105 => Self::InputAcceptedResponse,
            106 => Self::WorktreeResponse,
            107 => Self::DiffResponse,
            108 => Self::SessionResponse,
            109 => Self::SessionsResponse,
            110 => Self::EventResponse,
            111 => Self::LogChunkResponse,
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
    Ok(Some(IncomingRequest::Standard(decode_request(
        kind, &payload,
    )?)))
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
        payload.len() <= u32::MAX as usize,
        "payload too large: {} bytes",
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

    let mut payload = vec![0_u8; payload_len];
    reader.read_exact(&mut payload).await?;
    Ok(Some((FrameHeader { version, kind }, payload)))
}

fn encode_request(request: &Request) -> Result<(MessageKind, Vec<u8>)> {
    let mut payload = Vec::new();
    let kind = match request {
        Request::GetDaemonInfo => MessageKind::GetDaemonInfoRequest,
        Request::ShutdownDaemon => MessageKind::ShutdownDaemonRequest,
        Request::CreateSession {
            workspace,
            task,
            agent,
        } => {
            put_string(&mut payload, workspace)?;
            put_string(&mut payload, task)?;
            put_string(&mut payload, agent)?;
            MessageKind::CreateSessionRequest
        }
        Request::CreateWorktree { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::CreateWorktreeRequest
        }
        Request::CleanupWorktree { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::CleanupWorktreeRequest
        }
        Request::KillSession { session_id, remove } => {
            put_string(&mut payload, session_id)?;
            put_bool(&mut payload, *remove);
            MessageKind::KillSessionRequest
        }
        Request::AttachSession { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::AttachSessionRequest
        }
        Request::DetachSession { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::DetachSessionRequest
        }
        Request::AttachInput { data } => {
            put_bytes(&mut payload, data)?;
            MessageKind::AttachInputRequest
        }
        Request::SendInput {
            session_id,
            data,
            source_session_id,
        } => {
            put_string(&mut payload, session_id)?;
            put_bytes(&mut payload, data)?;
            put_optional_string(&mut payload, source_session_id.as_deref())?;
            MessageKind::SendInputRequest
        }
        Request::SwitchAttachedSession {
            source_session_id,
            target_session_id,
        } => {
            put_string(&mut payload, source_session_id)?;
            put_string(&mut payload, target_session_id)?;
            MessageKind::SwitchAttachedSessionRequest
        }
        Request::DiffSession { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::DiffSessionRequest
        }
        Request::GetSession { session_id } => {
            put_string(&mut payload, session_id)?;
            MessageKind::GetSessionRequest
        }
        Request::ListSessions => MessageKind::ListSessionsRequest,
        Request::AppendSessionEvents { session_id, events } => {
            put_string(&mut payload, session_id)?;
            put_len(&mut payload, events.len())?;
            for event in events {
                put_new_session_event(&mut payload, event)?;
            }
            MessageKind::AppendSessionEventsRequest
        }
        Request::StreamLogs { session_id, follow } => {
            put_string(&mut payload, session_id)?;
            put_bool(&mut payload, *follow);
            MessageKind::StreamLogsRequest
        }
        Request::StreamEvents { session_id, follow } => {
            put_string(&mut payload, session_id)?;
            put_bool(&mut payload, *follow);
            MessageKind::StreamEventsRequest
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
            workspace: cursor.take_string()?,
            task: cursor.take_string()?,
            agent: cursor.take_string()?,
        },
        MessageKind::CreateWorktreeRequest => Request::CreateWorktree {
            session_id: cursor.take_string()?,
        },
        MessageKind::CleanupWorktreeRequest => Request::CleanupWorktree {
            session_id: cursor.take_string()?,
        },
        MessageKind::KillSessionRequest => Request::KillSession {
            session_id: cursor.take_string()?,
            remove: cursor.take_bool()?,
        },
        MessageKind::AttachSessionRequest => Request::AttachSession {
            session_id: cursor.take_string()?,
        },
        MessageKind::DetachSessionRequest => Request::DetachSession {
            session_id: cursor.take_string()?,
        },
        MessageKind::AttachInputRequest => Request::AttachInput {
            data: cursor.take_bytes()?,
        },
        MessageKind::SendInputRequest => Request::SendInput {
            session_id: cursor.take_string()?,
            data: cursor.take_bytes()?,
            source_session_id: cursor.take_optional_string()?,
        },
        MessageKind::SwitchAttachedSessionRequest => Request::SwitchAttachedSession {
            source_session_id: cursor.take_string()?,
            target_session_id: cursor.take_string()?,
        },
        MessageKind::DiffSessionRequest => Request::DiffSession {
            session_id: cursor.take_string()?,
        },
        MessageKind::GetSessionRequest => Request::GetSession {
            session_id: cursor.take_string()?,
        },
        MessageKind::ListSessionsRequest => Request::ListSessions,
        MessageKind::AppendSessionEventsRequest => {
            let session_id = cursor.take_string()?;
            let len = cursor.take_len()?;
            let mut events = Vec::with_capacity(len);
            for _ in 0..len {
                events.push(cursor.take_new_session_event()?);
            }
            Request::AppendSessionEvents { session_id, events }
        }
        MessageKind::StreamLogsRequest => Request::StreamLogs {
            session_id: cursor.take_string()?,
            follow: cursor.take_bool()?,
        },
        MessageKind::StreamEventsRequest => Request::StreamEvents {
            session_id: cursor.take_string()?,
            follow: cursor.take_bool()?,
        },
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
        Response::KillSession {
            removed,
            was_running,
        } => {
            put_bool(&mut payload, *removed);
            put_bool(&mut payload, *was_running);
            MessageKind::KillSessionResponse
        }
        Response::Attached { snapshot } => {
            put_bytes(&mut payload, snapshot)?;
            MessageKind::AttachedResponse
        }
        Response::SessionEnded {
            session_id,
            status,
            exit_code,
            error,
        } => {
            put_string(&mut payload, session_id)?;
            put_session_status(&mut payload, *status);
            put_optional_i32(&mut payload, *exit_code);
            put_optional_string(&mut payload, error.as_deref())?;
            MessageKind::SessionEndedResponse
        }
        Response::InputAccepted => MessageKind::InputAcceptedResponse,
        Response::Worktree { worktree } => {
            put_worktree_record(&mut payload, worktree)?;
            MessageKind::WorktreeResponse
        }
        Response::Diff { diff } => {
            put_session_diff(&mut payload, diff)?;
            MessageKind::DiffResponse
        }
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
        Response::Event { event } => {
            put_session_event(&mut payload, event)?;
            MessageKind::EventResponse
        }
        Response::LogChunk { data } => {
            put_string(&mut payload, data)?;
            MessageKind::LogChunkResponse
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
        MessageKind::DaemonInfoResponse => Response::DaemonInfo {
            info: cursor.take_daemon_info()?,
        },
        MessageKind::CreateSessionResponse => Response::CreateSession {
            session: cursor.take_create_session_result()?,
        },
        MessageKind::KillSessionResponse => Response::KillSession {
            removed: cursor.take_bool()?,
            was_running: cursor.take_bool()?,
        },
        MessageKind::AttachedResponse => Response::Attached {
            snapshot: cursor.take_bytes()?,
        },
        MessageKind::SessionEndedResponse => Response::SessionEnded {
            session_id: cursor.take_string()?,
            status: cursor.take_session_status()?,
            exit_code: cursor.take_optional_i32()?,
            error: cursor.take_optional_string()?,
        },
        MessageKind::InputAcceptedResponse => Response::InputAccepted,
        MessageKind::WorktreeResponse => Response::Worktree {
            worktree: cursor.take_worktree_record()?,
        },
        MessageKind::DiffResponse => Response::Diff {
            diff: cursor.take_session_diff()?,
        },
        MessageKind::SessionResponse => Response::Session {
            session: cursor.take_session_record()?,
        },
        MessageKind::SessionsResponse => {
            let len = cursor.take_len()?;
            let mut sessions = Vec::with_capacity(len);
            for _ in 0..len {
                sessions.push(cursor.take_session_record()?);
            }
            Response::Sessions { sessions }
        }
        MessageKind::EventResponse => Response::Event {
            event: cursor.take_session_event()?,
        },
        MessageKind::LogChunkResponse => Response::LogChunk {
            data: cursor.take_string()?,
        },
        MessageKind::PtyOutputResponse => Response::PtyOutput {
            data: cursor.take_bytes()?,
        },
        MessageKind::SwitchSessionResponse => Response::SwitchSession {
            session_id: cursor.take_string()?,
        },
        MessageKind::EndOfStreamResponse => Response::EndOfStream,
        MessageKind::ErrorResponse => Response::Error {
            message: cursor.take_string()?,
        },
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
            Ok(DaemonManagementRequest::Shutdown {
                force: payload.force,
            })
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
        DaemonManagementResponse::Shutdown {
            stopped,
            running_sessions,
            message,
        } => (
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
        DAEMON_STATUS_RESPONSE_KIND => Ok(DaemonManagementResponse::Status {
            status: serde_json::from_slice(payload)?,
        }),
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
            Ok(DaemonManagementResponse::Error {
                message: payload.message,
            })
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
        SessionStatus::Paused => 3,
        SessionStatus::Exited => 4,
        SessionStatus::Failed => 5,
        SessionStatus::UnknownRecovered => 6,
    });
}

fn put_create_session_result(buf: &mut Vec<u8>, session: &CreateSessionResult) -> Result<()> {
    put_string(buf, &session.session_id)?;
    put_string(buf, &session.base_branch)?;
    put_string(buf, &session.branch)?;
    put_string(buf, &session.worktree)?;
    put_session_status(buf, session.status);
    Ok(())
}

fn put_worktree_record(buf: &mut Vec<u8>, worktree: &WorktreeRecord) -> Result<()> {
    put_string(buf, &worktree.session_id)?;
    put_string(buf, &worktree.repo_path)?;
    put_string(buf, &worktree.base_branch)?;
    put_string(buf, &worktree.branch)?;
    put_string(buf, &worktree.worktree)?;
    Ok(())
}

fn put_session_diff(buf: &mut Vec<u8>, diff: &SessionDiff) -> Result<()> {
    put_string(buf, &diff.session_id)?;
    put_string(buf, &diff.base_branch)?;
    put_string(buf, &diff.branch)?;
    put_string(buf, &diff.worktree)?;
    put_string(buf, &diff.diff)?;
    Ok(())
}

fn put_session_record(buf: &mut Vec<u8>, session: &SessionRecord) -> Result<()> {
    put_string(buf, &session.session_id)?;
    put_string(buf, &session.agent)?;
    put_string(buf, &session.workspace)?;
    put_string(buf, &session.repo_path)?;
    put_string(buf, &session.task)?;
    put_string(buf, &session.base_branch)?;
    put_string(buf, &session.branch)?;
    put_string(buf, &session.worktree)?;
    put_session_status(buf, session.status);
    put_optional_u32(buf, session.pid);
    put_optional_i32(buf, session.exit_code);
    put_optional_string(buf, session.error.as_deref())?;
    put_datetime(buf, &session.created_at);
    put_datetime(buf, &session.updated_at);
    put_optional_datetime(buf, session.exited_at.as_ref());
    Ok(())
}

fn put_session_event(buf: &mut Vec<u8>, event: &SessionEvent) -> Result<()> {
    buf.extend_from_slice(&event.id.to_le_bytes());
    put_string(buf, &event.session_id)?;
    put_datetime(buf, &event.timestamp);
    put_string(buf, &event.event_type)?;
    put_json_value(buf, &event.payload_json)?;
    Ok(())
}

fn put_new_session_event(buf: &mut Vec<u8>, event: &NewSessionEvent) -> Result<()> {
    put_string(buf, &event.event_type)?;
    put_json_value(buf, &event.payload_json)?;
    Ok(())
}

fn put_json_value(buf: &mut Vec<u8>, value: &serde_json::Value) -> Result<()> {
    put_string(buf, &serde_json::to_string(value)?)
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
            buf.extend_from_slice(&value.to_le_bytes());
        }
        None => buf.push(0),
    }
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
        Self {
            payload,
            position: 0,
        }
    }

    fn finish(&self) -> Result<()> {
        ensure!(
            self.position == self.payload.len(),
            "unexpected trailing payload bytes"
        );
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
        Ok(if self.take_bool()? {
            Some(self.take_string()?)
        } else {
            None
        })
    }

    fn take_optional_u32(&mut self) -> Result<Option<u32>> {
        Ok(if self.take_bool()? {
            Some(self.take_u32()?)
        } else {
            None
        })
    }

    fn take_optional_i32(&mut self) -> Result<Option<i32>> {
        Ok(if self.take_bool()? {
            Some(self.take_i32()?)
        } else {
            None
        })
    }

    fn take_len(&mut self) -> Result<usize> {
        Ok(self.take_u32()? as usize)
    }

    fn take_session_status(&mut self) -> Result<SessionStatus> {
        Ok(match self.take_u8()? {
            1 => SessionStatus::Creating,
            2 => SessionStatus::Running,
            3 => SessionStatus::Paused,
            4 => SessionStatus::Exited,
            5 => SessionStatus::Failed,
            6 => SessionStatus::UnknownRecovered,
            other => bail!("invalid session status `{other}`"),
        })
    }

    fn take_datetime(&mut self) -> Result<DateTime<Utc>> {
        let seconds = self.take_i64()?;
        let nanos = self.take_u32()?;
        Utc.timestamp_opt(seconds, nanos)
            .single()
            .ok_or_else(|| anyhow!("invalid UTC timestamp"))
    }

    fn take_optional_datetime(&mut self) -> Result<Option<DateTime<Utc>>> {
        Ok(if self.take_bool()? {
            Some(self.take_datetime()?)
        } else {
            None
        })
    }

    fn take_json_value(&mut self) -> Result<serde_json::Value> {
        serde_json::from_str(&self.take_string()?).map_err(Into::into)
    }

    fn take_daemon_info(&mut self) -> Result<DaemonInfo> {
        Ok(DaemonInfo {
            daemon_version: self.take_string()?,
            protocol_version: self.take_u16()?,
        })
    }

    fn take_create_session_result(&mut self) -> Result<CreateSessionResult> {
        Ok(CreateSessionResult {
            session_id: self.take_string()?,
            base_branch: self.take_string()?,
            branch: self.take_string()?,
            worktree: self.take_string()?,
            status: self.take_session_status()?,
        })
    }

    fn take_worktree_record(&mut self) -> Result<WorktreeRecord> {
        Ok(WorktreeRecord {
            session_id: self.take_string()?,
            repo_path: self.take_string()?,
            base_branch: self.take_string()?,
            branch: self.take_string()?,
            worktree: self.take_string()?,
        })
    }

    fn take_session_diff(&mut self) -> Result<SessionDiff> {
        Ok(SessionDiff {
            session_id: self.take_string()?,
            base_branch: self.take_string()?,
            branch: self.take_string()?,
            worktree: self.take_string()?,
            diff: self.take_string()?,
        })
    }

    fn take_session_record(&mut self) -> Result<SessionRecord> {
        Ok(SessionRecord {
            session_id: self.take_string()?,
            agent: self.take_string()?,
            workspace: self.take_string()?,
            repo_path: self.take_string()?,
            task: self.take_string()?,
            base_branch: self.take_string()?,
            branch: self.take_string()?,
            worktree: self.take_string()?,
            status: self.take_session_status()?,
            pid: self.take_optional_u32()?,
            exit_code: self.take_optional_i32()?,
            error: self.take_optional_string()?,
            created_at: self.take_datetime()?,
            updated_at: self.take_datetime()?,
            exited_at: self.take_optional_datetime()?,
        })
    }

    fn take_session_event(&mut self) -> Result<SessionEvent> {
        Ok(SessionEvent {
            id: self.take_i64()?,
            session_id: self.take_string()?,
            timestamp: self.take_datetime()?,
            event_type: self.take_string()?,
            payload_json: self.take_json_value()?,
        })
    }

    fn take_new_session_event(&mut self) -> Result<NewSessionEvent> {
        Ok(NewSessionEvent {
            event_type: self.take_string()?,
            payload_json: self.take_json_value()?,
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
        let end = self
            .position
            .checked_add(len)
            .ok_or_else(|| anyhow!("payload length overflow"))?;
        ensure!(end <= self.payload.len(), "truncated payload");
        let bytes = &self.payload[self.position..end];
        self.position = end;
        Ok(bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::{
        DAEMON_MANAGEMENT_VERSION, DaemonInfo, DaemonManagementRequest,
        DaemonManagementResponse, DaemonManagementStatus, IncomingRequest, PROTOCOL_VERSION,
        Request, Response, decode_request, decode_response, encode_request, encode_response,
        read_incoming_request, read_request, write_daemon_management_request,
        write_daemon_management_response,
    };
    use crate::{
        event::{NewSessionEvent, SessionEvent},
        session::{CreateSessionResult, SessionRecord, SessionStatus},
    };
    use chrono::Utc;
    use serde_json::json;
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
        let request = Request::DetachSession {
            session_id: "demo".to_string(),
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn response_round_trips_snapshot_bytes() {
        let response = Response::Attached {
            snapshot: vec![0, 1, 2, 3, 4],
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn switch_session_response_round_trips() {
        let response = Response::SwitchSession {
            session_id: "target".to_string(),
        };
        let (kind, payload) = encode_response(&response).unwrap();
        let decoded = decode_response(kind, &payload).unwrap();
        assert_eq!(decoded, response);
    }

    #[test]
    fn event_response_round_trips() {
        let response = Response::Event {
            event: SessionEvent {
                id: 7,
                session_id: "demo".to_string(),
                timestamp: Utc::now(),
                event_type: "COMMAND_EXECUTED".to_string(),
                payload_json: json!({"command":"cargo test","exit_code":1}),
            },
        };
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
                workspace: "/tmp/demo".to_string(),
                repo_path: "/tmp/demo".to_string(),
                task: "fix".to_string(),
                base_branch: "main".to_string(),
                branch: "agent/fix".to_string(),
                worktree: "/tmp/worktree".to_string(),
                status: SessionStatus::Running,
                pid: Some(123),
                exit_code: None,
                error: None,
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
        write_daemon_management_request(&mut writer, &request)
            .await
            .unwrap();
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
        write_daemon_management_response(&mut writer, &response)
            .await
            .unwrap();
        drop(writer);

        let decoded = super::read_daemon_management_response(&mut reader)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(decoded, response);
    }

    #[tokio::test]
    async fn incoming_request_accepts_daemon_management_version() {
        let (mut writer, mut reader) = tokio::io::duplex(1024);
        writer
            .write_all(&super::FRAME_MAGIC.to_le_bytes())
            .await
            .unwrap();
        writer
            .write_all(&DAEMON_MANAGEMENT_VERSION.to_le_bytes())
            .await
            .unwrap();
        writer
            .write_all(&super::DAEMON_STATUS_REQUEST_KIND.to_le_bytes())
            .await
            .unwrap();
        writer.write_all(&0_u16.to_le_bytes()).await.unwrap();
        writer.write_all(&0_u16.to_le_bytes()).await.unwrap();
        writer.write_all(&0_u32.to_le_bytes()).await.unwrap();
        drop(writer);

        let incoming = read_incoming_request(&mut reader).await.unwrap().unwrap();
        assert_eq!(
            incoming,
            IncomingRequest::DaemonManagement(DaemonManagementRequest::Status)
        );
    }

    #[test]
    fn append_session_events_round_trips() {
        let request = Request::AppendSessionEvents {
            session_id: "demo".to_string(),
            events: vec![NewSessionEvent {
                event_type: "progress".to_string(),
                payload_json: json!({"value":42}),
            }],
        };
        let (kind, payload) = encode_request(&request).unwrap();
        let decoded = decode_request(kind, &payload).unwrap();
        assert_eq!(decoded, request);
    }

    #[test]
    fn create_session_result_round_trips() {
        let response = Response::CreateSession {
            session: CreateSessionResult {
                session_id: "demo".to_string(),
                base_branch: "main".to_string(),
                branch: "agent/demo".to_string(),
                worktree: "/tmp/demo".to_string(),
                status: SessionStatus::Running,
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
}
