use std::{
    cell::RefCell,
    collections::HashMap,
    fs,
    io::{Read, Write},
    rc::Rc,
    sync::mpsc as std_mpsc,
    thread,
};

use anyhow::{Context, Result, anyhow, bail};
use chrono::Utc;
use libghostty_vt::{
    Terminal, TerminalOptions,
    fmt::{Format as FormatterFormat, Formatter, FormatterOptions},
};
use nix::{
    errno::Errno,
    sys::signal::{Signal, kill},
    unistd::Pid,
};
use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use tokio::{
    io::BufReader,
    net::{UnixListener, UnixStream, unix::OwnedWriteHalf},
    sync::{broadcast, mpsc, watch},
};

use agentd_shared::{
    paths::AppPaths,
    protocol::{Request, Response, read_request, write_response},
    session::{ApplyState, AttachmentKind, AttachmentRecord, SessionStatus},
};

use crate::{SessionWorkerArgs, db::Database};

const DEFAULT_PTY_ROWS: u16 = 48;
const DEFAULT_PTY_COLS: u16 = 160;
const MAX_SCROLLBACK_BYTES: usize = 10_000_000;

pub async fn run(args: SessionWorkerArgs) -> Result<()> {
    let paths = AppPaths::discover()?;
    paths.ensure_layout()?;
    let db = Database::open(&paths)?;
    let socket_path = paths.session_socket_path(&args.session_id);

    match fs::remove_file(socket_path.as_std_path()) {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
        Err(err) => return Err(err).with_context(|| format!("failed to remove {}", socket_path)),
    }

    let pty_system = native_pty_system();
    let pair = pty_system
        .openpty(PtySize {
            rows: DEFAULT_PTY_ROWS,
            cols: DEFAULT_PTY_COLS,
            pixel_width: 0,
            pixel_height: 0,
        })
        .context("failed to allocate PTY")?;

    let mut command = CommandBuilder::new(&args.command);
    command.args(args.args.clone());
    command.cwd(&args.worktree);
    for (key, value) in std::env::vars() {
        command.env(&key, &value);
    }
    command.env("AGENTD_SESSION_ID", &args.session_id);
    command.env("AGENTD_SESSION_NAME", &args.session_id);
    command.env("AGENTD_SOCKET", paths.socket.as_str());
    command.env("AGENTD_WORKSPACE", &args.repo_root);
    command.env("AGENTD_WORKTREE", &args.worktree);
    command.env("AGENTD_BRANCH", &args.branch);

    let mut child = pair
        .slave
        .spawn_command(command)
        .map_err(|err| anyhow!(err).context("failed to spawn agent process"))?;
    let agent_pid = child.process_id().map(|value| value as u32).unwrap_or(0);
    let worker_pid = std::process::id();

    let listener =
        UnixListener::bind(socket_path.as_std_path()).context("failed to bind worker socket")?;
    db.mark_running(&args.session_id, worker_pid, agent_pid)?;

    let (output_tx, _) = broadcast::channel(256);
    let (ended_tx, ended_rx) = watch::channel(None);
    let (command_tx, command_rx) = std_mpsc::channel();

    let mut master = pair.master;
    let reader = master.try_clone_reader().context("failed to clone PTY reader")?;
    let writer = master.take_writer().context("failed to take PTY writer")?;
    let owner_handle = thread::spawn({
        let db = db.clone();
        let paths = paths.clone();
        let session_id = args.session_id.clone();
        let output_tx = output_tx.clone();
        let ended_tx = ended_tx.clone();
        move || {
            owner_loop(
                db,
                paths,
                session_id,
                master,
                writer,
                command_rx,
                output_tx,
                ended_tx,
            )
        }
    });

    let command_tx_reader = command_tx.clone();
    thread::spawn(move || {
        let _ = pump_pty(reader, &command_tx_reader);
    });

    let command_tx_wait = command_tx.clone();
    thread::spawn(move || {
        let status = child.wait();
        let code = status.ok().map(|value| value.exit_code() as i32);
        let _ = command_tx_wait.send(OwnerCommand::ChildExited(code));
    });

    let mut ended_rx_main = ended_rx.clone();
    loop {
        tokio::select! {
            accept_result = listener.accept() => {
                let (stream, _) = accept_result?;
                let command_tx = command_tx.clone();
                let output_tx = output_tx.clone();
                let ended_rx = ended_rx.clone();
                let session_id = args.session_id.clone();
                tokio::spawn(async move {
                    if let Err(err) = handle_connection(stream, &session_id, command_tx, output_tx, ended_rx).await {
                        eprintln!("session worker connection error: {err:#}");
                    }
                });
            }
            changed = ended_rx_main.changed() => {
                if changed.is_err() {
                    break;
                }
                if ended_rx_main.borrow().is_some() {
                    break;
                }
            }
        }
    }

    drop(listener);
    let _ = fs::remove_file(socket_path.as_std_path());
    let _ = owner_handle.join();
    Ok(())
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct TerminalGeometry {
    cols: u16,
    rows: u16,
    pixel_width: u16,
    pixel_height: u16,
}

impl TerminalGeometry {
    fn cell_width_px(self) -> u32 {
        if self.cols == 0 || self.pixel_width == 0 {
            0
        } else {
            u32::from(self.pixel_width) / u32::from(self.cols)
        }
    }

    fn cell_height_px(self) -> u32 {
        if self.rows == 0 || self.pixel_height == 0 {
            0
        } else {
            u32::from(self.pixel_height) / u32::from(self.rows)
        }
    }

    fn into_pty_size(self) -> PtySize {
        PtySize {
            rows: self.rows,
            cols: self.cols,
            pixel_width: self.pixel_width,
            pixel_height: self.pixel_height,
        }
    }
}

#[derive(Clone, Debug)]
struct SessionEnded {
    status: SessionStatus,
    apply_state: ApplyState,
    has_commits: bool,
    branch: String,
    worktree: String,
    exit_code: Option<i32>,
    error: Option<String>,
}

enum OwnerCommand {
    PtyOutput(Vec<u8>),
    Attach {
        kind: AttachmentKind,
        geometry: TerminalGeometry,
        control_tx: mpsc::UnboundedSender<()>,
        response_tx: tokio::sync::oneshot::Sender<Result<(String, Vec<u8>, chrono::DateTime<Utc>)>>,
    },
    RemoveAttachment {
        attach_id: String,
    },
    ListAttachments {
        response_tx: tokio::sync::oneshot::Sender<Result<Vec<AttachmentRecord>>>,
    },
    DetachAttachment {
        attach_id: String,
        response_tx: tokio::sync::oneshot::Sender<Result<()>>,
    },
    DetachAll {
        response_tx: tokio::sync::oneshot::Sender<Result<()>>,
    },
    History {
        vt: bool,
        response_tx: tokio::sync::oneshot::Sender<Result<String>>,
    },
    Snapshot {
        response_tx: tokio::sync::oneshot::Sender<Result<Vec<u8>>>,
    },
    Resize {
        geometry: TerminalGeometry,
        response_tx: tokio::sync::oneshot::Sender<Result<()>>,
    },
    Input {
        data: Vec<u8>,
        response_tx: tokio::sync::oneshot::Sender<Result<()>>,
    },
    Terminate {
        response_tx: tokio::sync::oneshot::Sender<Result<()>>,
    },
    ChildExited(Option<i32>),
}

struct OwnerAttachment {
    kind: AttachmentKind,
    connected_at: chrono::DateTime<Utc>,
    control_tx: mpsc::UnboundedSender<()>,
}

fn owner_loop(
    db: Database,
    paths: AppPaths,
    session_id: String,
    master: Box<dyn MasterPty + Send>,
    mut writer: Box<dyn Write + Send>,
    command_rx: std_mpsc::Receiver<OwnerCommand>,
    output_tx: broadcast::Sender<Vec<u8>>,
    ended_tx: watch::Sender<Option<SessionEnded>>,
) -> Result<()> {
    let pending_writes = Rc::new(RefCell::new(Vec::<Vec<u8>>::new()));
    let mut terminal = Terminal::new(TerminalOptions {
        cols: DEFAULT_PTY_COLS,
        rows: DEFAULT_PTY_ROWS,
        max_scrollback: MAX_SCROLLBACK_BYTES,
    })?;
    terminal.on_pty_write({
        let pending_writes = pending_writes.clone();
        move |_term, data| {
            pending_writes.borrow_mut().push(data.to_vec());
        }
    })?;

    let mut state = OwnerState {
        session_id: session_id.clone(),
        master,
        writer,
        terminal,
        pending_writes,
        has_client_dimensions: false,
        geometry: TerminalGeometry {
            cols: DEFAULT_PTY_COLS,
            rows: DEFAULT_PTY_ROWS,
            pixel_width: 0,
            pixel_height: 0,
        },
        next_attach_ordinal: 1,
        attachments: HashMap::new(),
    };

    while let Ok(command) = command_rx.recv() {
        match command {
            OwnerCommand::PtyOutput(data) => state.publish_output(&data, &output_tx)?,
            OwnerCommand::Attach { kind, geometry, control_tx, response_tx } => {
                let result = state.attach(kind, geometry, control_tx);
                let _ = response_tx.send(result);
            }
            OwnerCommand::RemoveAttachment { attach_id } => {
                state.attachments.remove(&attach_id);
            }
            OwnerCommand::ListAttachments { response_tx } => {
                let _ = response_tx.send(Ok(state.list_attachments()));
            }
            OwnerCommand::DetachAttachment { attach_id, response_tx } => {
                let _ = response_tx.send(state.detach_attachment(&attach_id));
            }
            OwnerCommand::DetachAll { response_tx } => {
                let _ = response_tx.send(state.detach_all());
            }
            OwnerCommand::History { vt, response_tx } => {
                let _ = response_tx.send(state.history(vt));
            }
            OwnerCommand::Snapshot { response_tx } => {
                let _ = response_tx.send(state.snapshot());
            }
            OwnerCommand::Resize { geometry, response_tx } => {
                let _ = response_tx.send(state.resize(geometry));
            }
            OwnerCommand::Input { data, response_tx } => {
                let _ = response_tx.send(state.write_input(&data));
            }
            OwnerCommand::Terminate { response_tx } => {
                let _ = response_tx.send(terminate_process_group(
                    &session_id,
                    db.get_session(&session_id)?.and_then(|s| s.agent_pid),
                ));
            }
            OwnerCommand::ChildExited(exit_code) => {
                let plain = state.history(false).unwrap_or_default();
                let vt = state.history(true).unwrap_or_default();
                fs::write(paths.rendered_log_path(&state.session_id).as_std_path(), &plain)
                    .with_context(|| format!("failed to write rendered history for {}", state.session_id))?;
                fs::write(paths.log_path(&state.session_id).as_std_path(), &vt)
                    .with_context(|| format!("failed to write VT history for {}", state.session_id))?;
                finalize_worker_exit(&db, &state.session_id, exit_code)?;
                let session = db.get_session(&state.session_id)?.ok_or_else(|| anyhow!("missing session after worker exit"))?;
                let _ = ended_tx.send(Some(SessionEnded {
                    status: session.status,
                    apply_state: session.apply_state,
                    has_commits: session.has_commits,
                    branch: session.branch,
                    worktree: session.worktree,
                    exit_code: session.exit_code,
                    error: session.error,
                }));
                break;
            }
        }
    }

    Ok(())
}

struct OwnerState {
    session_id: String,
    master: Box<dyn MasterPty + Send>,
    writer: Box<dyn Write + Send>,
    terminal: Terminal<'static, 'static>,
    pending_writes: Rc<RefCell<Vec<Vec<u8>>>>,
    has_client_dimensions: bool,
    geometry: TerminalGeometry,
    next_attach_ordinal: u64,
    attachments: HashMap<String, OwnerAttachment>,
}

impl OwnerState {
    fn has_live_attach_terminal(&self) -> bool {
        self.attachments.values().any(|attachment| attachment.kind == AttachmentKind::Attach)
    }

    fn write_input(&mut self, data: &[u8]) -> Result<()> {
        self.writer.write_all(data)?;
        self.writer.flush()?;
        Ok(())
    }

    fn resize(&mut self, geometry: TerminalGeometry) -> Result<()> {
        self.master.resize(geometry.into_pty_size())?;
        self.terminal.resize(
            geometry.cols,
            geometry.rows,
            geometry.cell_width_px(),
            geometry.cell_height_px(),
        )?;
        self.has_client_dimensions = true;
        self.geometry = geometry;
        Ok(())
    }

    fn publish_output(&mut self, data: &[u8], output_tx: &broadcast::Sender<Vec<u8>>) -> Result<()> {
        self.terminal.vt_write(data);
        let writes = self.pending_writes.replace(Vec::new());
        if !self.has_live_attach_terminal() {
            for response in writes {
                self.write_input(&response)?;
            }
        }
        let _ = output_tx.send(data.to_vec());
        Ok(())
    }

    fn history(&mut self, vt: bool) -> Result<String> {
        let mut formatter = Formatter::new(
            &self.terminal,
            FormatterOptions {
                format: if vt { FormatterFormat::Vt } else { FormatterFormat::Plain },
                trim: false,
                unwrap: false,
            },
        )?;
        let bytes = formatter.format_alloc::<()>(None)?;
        Ok(String::from_utf8_lossy(bytes.as_ref()).into_owned())
    }

    fn snapshot(&mut self) -> Result<Vec<u8>> {
        let mut formatter = Formatter::new(
            &self.terminal,
            FormatterOptions { format: FormatterFormat::Vt, trim: false, unwrap: false },
        )?;
        Ok(formatter.format_alloc::<()>(None)?.as_ref().to_vec())
    }

    fn attach(
        &mut self,
        kind: AttachmentKind,
        geometry: TerminalGeometry,
        control_tx: mpsc::UnboundedSender<()>,
    ) -> Result<(String, Vec<u8>, chrono::DateTime<Utc>)> {
        let attach_id = format!("{}-{}", kind.as_str(), self.next_attach_ordinal);
        self.next_attach_ordinal += 1;
        let connected_at = Utc::now();
        let had_client_dimensions = self.has_client_dimensions;
        let snapshot = if had_client_dimensions {
            let snapshot = self.snapshot()?;
            self.resize(geometry)?;
            snapshot
        } else {
            self.resize(geometry)?;
            self.snapshot()?
        };
        self.attachments.insert(
            attach_id.clone(),
            OwnerAttachment { kind, connected_at, control_tx },
        );
        Ok((attach_id, snapshot, connected_at))
    }

    fn list_attachments(&self) -> Vec<AttachmentRecord> {
        let mut attachments = self
            .attachments
            .iter()
            .map(|(attach_id, attachment)| AttachmentRecord {
                attach_id: attach_id.clone(),
                session_id: self.session_id.clone(),
                kind: attachment.kind,
                connected_at: attachment.connected_at,
            })
            .collect::<Vec<_>>();
        attachments.sort_by(|left, right| left.connected_at.cmp(&right.connected_at));
        attachments
    }

    fn detach_attachment(&self, attach_id: &str) -> Result<()> {
        let attachment = self
            .attachments
            .get(attach_id)
            .ok_or_else(|| anyhow!("attachment `{attach_id}` not found"))?;
        attachment
            .control_tx
            .send(())
            .map_err(|_| anyhow!("attachment `{attach_id}` is no longer connected"))?;
        Ok(())
    }

    fn detach_all(&self) -> Result<()> {
        for (attach_id, attachment) in &self.attachments {
            attachment
                .control_tx
                .send(())
                .map_err(|_| anyhow!("attachment `{attach_id}` is no longer connected"))?;
        }
        Ok(())
    }
}

async fn handle_connection(
    stream: UnixStream,
    session_id: &str,
    command_tx: std_mpsc::Sender<OwnerCommand>,
    output_tx: broadcast::Sender<Vec<u8>>,
    ended_rx: watch::Receiver<Option<SessionEnded>>,
) -> Result<()> {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);
    let Some(request) = read_request(&mut reader).await? else {
        return Ok(());
    };

    match request {
        Request::AttachSession { session_id: _, kind, cols, rows, pixel_width, pixel_height } => {
            serve_attach_connection(
                session_id,
                kind,
                TerminalGeometry { cols, rows, pixel_width, pixel_height },
                &mut reader,
                &mut writer,
                command_tx,
                output_tx,
                ended_rx,
            )
            .await
        }
        Request::SendInput { data, .. } | Request::AttachInput { data } => {
            let (tx, rx) = tokio::sync::oneshot::channel();
            command_tx.send(OwnerCommand::Input { data, response_tx: tx })?;
            rx.await??;
            write_response(&mut writer, &Response::InputAccepted).await
        }
        Request::ListAttachments { .. } => {
            let (tx, rx) = tokio::sync::oneshot::channel();
            command_tx.send(OwnerCommand::ListAttachments { response_tx: tx })?;
            write_response(&mut writer, &Response::Attachments { attachments: rx.await?? }).await
        }
        Request::DetachAttachment { attach_id, .. } => {
            let (tx, rx) = tokio::sync::oneshot::channel();
            command_tx.send(OwnerCommand::DetachAttachment { attach_id, response_tx: tx })?;
            rx.await??;
            write_response(&mut writer, &Response::Ok).await
        }
        Request::DetachSession { all, .. } => {
            if !all {
                bail!("worker detach requires all=true");
            }
            let (tx, rx) = tokio::sync::oneshot::channel();
            command_tx.send(OwnerCommand::DetachAll { response_tx: tx })?;
            rx.await??;
            write_response(&mut writer, &Response::Ok).await
        }
        Request::GetHistory { vt, .. } => {
            let (tx, rx) = tokio::sync::oneshot::channel();
            command_tx.send(OwnerCommand::History { vt, response_tx: tx })?;
            write_response(&mut writer, &Response::History { data: rx.await?? }).await
        }
        Request::KillSession { .. } => {
            let (tx, rx) = tokio::sync::oneshot::channel();
            command_tx.send(OwnerCommand::Terminate { response_tx: tx })?;
            rx.await??;
            write_response(&mut writer, &Response::Ok).await
        }
        other => {
            write_response(
                &mut writer,
                &Response::Error { message: format!("unsupported worker request: {other:?}") },
            )
            .await
        }
    }
}

async fn serve_attach_connection(
    session_id: &str,
    kind: AttachmentKind,
    geometry: TerminalGeometry,
    reader: &mut BufReader<tokio::net::unix::OwnedReadHalf>,
    writer: &mut OwnedWriteHalf,
    command_tx: std_mpsc::Sender<OwnerCommand>,
    output_tx: broadcast::Sender<Vec<u8>>,
    mut ended_rx: watch::Receiver<Option<SessionEnded>>,
) -> Result<()> {
    let (control_tx, mut control_rx) = mpsc::unbounded_channel();
    let (tx, rx) = tokio::sync::oneshot::channel();
    command_tx.send(OwnerCommand::Attach { kind, geometry, control_tx, response_tx: tx })?;
    let (attach_id, snapshot, connected_at) = rx.await??;
    let mut output_rx = output_tx.subscribe();

    write_response(writer, &Response::Attached { attach_id: attach_id.clone(), snapshot }).await?;
    let mut final_response = Some(Response::EndOfStream);

    loop {
        tokio::select! {
            output = output_rx.recv() => match output {
                Ok(data) => write_response(writer, &Response::PtyOutput { data }).await?,
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => break,
            },
            control = control_rx.recv() => match control {
                Some(()) => break,
                None => break,
            },
            changed = ended_rx.changed() => {
                if changed.is_ok() {
                    if let Some(ended) = ended_rx.borrow().clone() {
                        final_response = Some(Response::SessionEnded {
                            session_id: session_id.to_string(),
                            status: ended.status,
                            apply_state: ended.apply_state,
                            has_commits: ended.has_commits,
                            branch: ended.branch,
                            worktree: ended.worktree,
                            exit_code: ended.exit_code,
                            error: ended.error,
                        });
                    }
                }
                break;
            }
            request = read_request(reader) => {
                let Some(request) = request? else {
                    break;
                };
                match request {
                    Request::AttachResize { cols, rows, pixel_width, pixel_height } => {
                        let (tx, rx) = tokio::sync::oneshot::channel();
                        command_tx.send(OwnerCommand::Resize {
                            geometry: TerminalGeometry { cols, rows, pixel_width, pixel_height },
                            response_tx: tx,
                        })?;
                        rx.await??;
                    }
                    Request::AttachInput { data } => {
                        let (tx, rx) = tokio::sync::oneshot::channel();
                        command_tx.send(OwnerCommand::Input { data, response_tx: tx })?;
                        rx.await??;
                    }
                    Request::AttachSnapshot => {
                        let (tx, rx) = tokio::sync::oneshot::channel();
                        command_tx.send(OwnerCommand::Snapshot { response_tx: tx })?;
                        write_response(writer, &Response::AttachSnapshot { snapshot: rx.await?? }).await?;
                    }
                    other => {
                        final_response = Some(Response::Error {
                            message: format!("unexpected request during attach: {other:?}"),
                        });
                        break;
                    }
                }
            }
        }
    }

    let _ = command_tx.send(OwnerCommand::RemoveAttachment { attach_id });
    let _ = connected_at;
    if let Some(response) = final_response {
        write_response(writer, &response).await?;
    }
    Ok(())
}

fn pump_pty(mut reader: Box<dyn Read + Send>, command_tx: &std_mpsc::Sender<OwnerCommand>) -> Result<()> {
    let mut buffer = [0_u8; 8192];
    loop {
        let bytes_read = reader.read(&mut buffer)?;
        if bytes_read == 0 {
            break;
        }
        command_tx.send(OwnerCommand::PtyOutput(buffer[..bytes_read].to_vec()))?;
    }
    Ok(())
}

fn finalize_worker_exit(db: &Database, session_id: &str, exit_code: Option<i32>) -> Result<()> {
    let Some(session) = db.get_session(session_id)? else {
        return Ok(());
    };
    if session.status == SessionStatus::Failed {
        return Ok(());
    }

    if exit_code == Some(0) {
        db.mark_exited(session_id, exit_code, ApplyState::Idle)?;
    } else {
        let error = match exit_code {
            Some(code) => format!("agent exited with code {code}"),
            None => "agent exited unexpectedly".to_string(),
        };
        db.mark_failed(session_id, error)?;
    }
    Ok(())
}

fn terminate_process_group(session_id: &str, agent_pid: Option<u32>) -> Result<()> {
    let pid = agent_pid.ok_or_else(|| anyhow!("session `{session_id}` has no recorded agent pid"))?;
    let pid = Pid::from_raw(pid as i32);
    match kill(pid, Some(Signal::SIGTERM)) {
        Ok(()) => Ok(()),
        Err(Errno::ESRCH) => Ok(()),
        Err(err) => Err(anyhow!(err)).context(format!("failed to terminate `{session_id}`")),
    }
}
