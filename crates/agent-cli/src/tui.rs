use std::time::Duration;

use anyhow::{Context, Result, bail};
use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use ratatui::{
    Frame, Terminal,
    backend::CrosstermBackend,
    layout::{Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear as WidgetClear, Paragraph, Wrap},
};
use tokio::{
    io::BufReader,
    net::UnixStream,
    sync::mpsc::{UnboundedReceiver, UnboundedSender, unbounded_channel},
};

use agentd_shared::{
    paths::AppPaths,
    protocol::{Request, Response, read_response, write_request},
    session::{AttachmentKind, IntegrationState, SessionRecord, SessionStatus},
};

use crate::{
    CODEX_MODELS, RawModeGuard, StatusString, TerminalScreenGuard, centered_rect,
    daemon_get_session, daemon_list_sessions, encode_attach_key, kill_session, send_request,
    session_display::session_elapsed_label,
};

const LEADER_KEY: (KeyCode, KeyModifiers) = (KeyCode::Char('b'), KeyModifiers::CONTROL);
const SIDEBAR_WIDTH: u16 = 34;
const CARD_HEIGHT: u16 = 6;
pub async fn run_runtime_ui(paths: &AppPaths, initial_session_id: Option<&str>) -> Result<()> {
    let _raw_mode = RawModeGuard::new()?;
    let _screen = TerminalScreenGuard::enter()?;
    let backend = CrosstermBackend::new(std::io::stdout());
    let mut terminal = Terminal::new(backend).context("failed to initialize terminal")?;
    let mut app = RuntimeApp::new(paths.clone(), initial_session_id);

    loop {
        app.refresh_sessions().await?;
        app.advance_timers();
        app.sync_focus_runtime().await?;

        terminal.draw(|frame| app.render(frame))?;

        app.drain_pty_messages();

        if !event::poll(Duration::from_millis(100)).context("failed to poll terminal input")? {
            continue;
        }

        match event::read().context("failed to read terminal input")? {
            Event::Key(key) if key.kind == KeyEventKind::Press => {
                if app.handle_key(key).await? {
                    break;
                }
            }
            Event::Resize(width, height) => {
                app.on_resize(width, height);
            }
            _ => {}
        }
    }

    Ok(())
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum FocusTarget {
    Coordinator,
    Worker(String),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Modal {
    None,
    Palette,
    WorktreeActions,
    GitStatus,
    Diff,
    StopConfirm,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ComposerField {
    Query,
    Agent,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum ComposerRow {
    Create,
    Session(String),
}

#[derive(Clone, Debug)]
struct InlineComposer {
    active: bool,
    query: String,
    agent_input: String,
    selected: usize,
    field: ComposerField,
    status_lines: Vec<String>,
}

#[derive(Clone, Debug)]
struct PaletteItem {
    key_hint: &'static str,
    title: &'static str,
    command: Command,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Command {
    NextSession,
    PreviousSession,
    NextAttention,
    FocusCoordinator,
    SessionSwitcher,
    NewSession,
    NewAgent,
    WorktreeActions,
    GitStatus,
    Diff,
    StopSession,
}

struct RuntimeApp {
    paths: AppPaths,
    sessions: Vec<SessionRecord>,
    focus: FocusTarget,
    leader_pending: bool,
    modal: Modal,
    palette_query: String,
    palette_selected: usize,
    composer: InlineComposer,
    diff_text: String,
    diff_scroll: u16,
    detail_text: String,
    detail_scroll: u16,
    toast: Option<String>,
    pending_focus: Option<(String, std::time::Instant)>,
    pty: Option<FocusedPty>,
    last_pane_size: Option<(u16, u16)>,
    focused_history_key: Option<(String, chrono::DateTime<chrono::Utc>)>,
    focused_history: String,
}

impl RuntimeApp {
    fn new(paths: AppPaths, initial_session_id: Option<&str>) -> Self {
        let focus = initial_session_id
            .map(|session_id| FocusTarget::Worker(session_id.to_string()))
            .unwrap_or(FocusTarget::Coordinator);
        Self {
            paths,
            sessions: Vec::new(),
            focus,
            leader_pending: false,
            modal: Modal::None,
            palette_query: String::new(),
            palette_selected: 0,
            composer: InlineComposer {
                active: initial_session_id.is_none(),
                query: String::new(),
                agent_input: "codex".to_string(),
                selected: 0,
                field: ComposerField::Query,
                status_lines: Vec::new(),
            },
            diff_text: String::new(),
            diff_scroll: 0,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
            pending_focus: None,
            pty: None,
            last_pane_size: None,
            focused_history_key: None,
            focused_history: String::new(),
        }
    }

    async fn refresh_sessions(&mut self) -> Result<()> {
        self.sessions = daemon_list_sessions(&self.paths).await?;
        let existing = self.sessions.iter().any(
            |session| matches!(&self.focus, FocusTarget::Worker(id) if id == &session.session_id),
        );
        if matches!(self.focus, FocusTarget::Worker(_)) && !existing {
            self.focus = FocusTarget::Coordinator;
        }
        self.clamp_composer_selection();
        self.refresh_focused_history().await?;
        Ok(())
    }

    async fn refresh_focused_history(&mut self) -> Result<()> {
        let target = self.focused_worker_session_id().and_then(|session_id| {
            self.sessions
                .iter()
                .find(|session| session.session_id == session_id)
                .map(|session| (session.session_id.clone(), session.updated_at, session.status))
        });
        let Some((session_id, updated_at, status)) = target else {
            self.focused_history_key = None;
            self.focused_history.clear();
            return Ok(());
        };

        if matches!(status, SessionStatus::Running | SessionStatus::NeedsInput) {
            self.focused_history_key = None;
            self.focused_history.clear();
            return Ok(());
        }

        let key = (session_id.clone(), updated_at);
        if self.focused_history_key.as_ref() == Some(&key) {
            return Ok(());
        }

        self.focused_history = super::fetch_focus_history(&self.paths, &session_id).await?;
        self.focused_history_key = Some(key);
        Ok(())
    }

    fn advance_timers(&mut self) {
        let should_switch = self
            .pending_focus
            .as_ref()
            .map(|(_, deadline)| std::time::Instant::now() >= *deadline)
            .unwrap_or(false);
        if should_switch {
            if let Some((session_id, _)) = self.pending_focus.take() {
                self.focus = FocusTarget::Worker(session_id);
                self.composer.active = false;
                self.composer.status_lines.clear();
            }
        }
    }

    async fn sync_focus_runtime(&mut self) -> Result<()> {
        let live_session_id = self.focused_worker_session_id().map(str::to_string);
        match live_session_id {
            Some(session_id) => {
                let live_session =
                    self.sessions.iter().find(|session| session.session_id == session_id);
                let live_status = live_session.map(|session| session.status);
                let needs_attach =
                    self.pty.as_ref().map(|pty| pty.session_id != session_id).unwrap_or(true);

                if needs_attach {
                    self.stop_pty();
                    if matches!(
                        live_status,
                        Some(SessionStatus::Running | SessionStatus::NeedsInput)
                    ) {
                        self.pty = Some(FocusedPty::spawn(self.paths.clone(), session_id.clone()));
                    }
                }

                if !matches!(live_status, Some(SessionStatus::Running | SessionStatus::NeedsInput))
                {
                    self.stop_pty();
                }
            }
            None => self.stop_pty(),
        }
        Ok(())
    }

    fn stop_pty(&mut self) {
        if let Some(pty) = &self.pty {
            let _ = pty.commands.send(PtyCommand::Close);
        }
        self.pty = None;
    }

    fn focused_worker_session_id(&self) -> Option<&str> {
        match &self.focus {
            FocusTarget::Coordinator => None,
            FocusTarget::Worker(session_id) => Some(session_id),
        }
    }

    fn focused_session(&self) -> Option<&SessionRecord> {
        let session_id = self.focused_worker_session_id()?;
        self.sessions.iter().find(|session| session.session_id == session_id)
    }

    fn ordered_sessions(&self) -> Vec<&SessionRecord> {
        let mut sessions = self.sessions.iter().collect::<Vec<_>>();
        sessions.sort_by(|left, right| {
            session_rank(left)
                .cmp(&session_rank(right))
                .then_with(|| right.updated_at.cmp(&left.updated_at))
        });
        sessions
    }

    fn ordered_targets(&self) -> Vec<FocusTarget> {
        let mut targets = vec![FocusTarget::Coordinator];
        targets.extend(
            self.ordered_sessions()
                .into_iter()
                .map(|session| FocusTarget::Worker(session.session_id.clone())),
        );
        targets
    }

    fn attention_targets(&self) -> Vec<FocusTarget> {
        self.ordered_sessions()
            .into_iter()
            .filter(|session| session_rank(session) <= 3)
            .map(|session| FocusTarget::Worker(session.session_id.clone()))
            .collect()
    }

    fn cycle_focus(&mut self, direction: isize, attention_only: bool) {
        let targets =
            if attention_only { self.attention_targets() } else { self.ordered_targets() };
        if targets.is_empty() {
            return;
        }
        let current_index = targets.iter().position(|target| *target == self.focus).unwrap_or(0);
        let len = targets.len() as isize;
        let next = (current_index as isize + direction).rem_euclid(len) as usize;
        self.focus = targets[next].clone();
    }

    fn open_composer(&mut self, field: ComposerField, prefer_create: bool) {
        self.focus = FocusTarget::Coordinator;
        self.composer.active = true;
        self.composer.field = field;
        if !prefer_create {
            self.composer.query.clear();
        }
        self.composer.status_lines.clear();
        self.pending_focus = None;
        self.clamp_composer_selection();
        if prefer_create {
            self.composer.selected = 0;
        } else if self.composer_rows().len() > 1 {
            self.composer.selected = 1;
        }
    }

    fn close_composer(&mut self) {
        self.composer.active = false;
        self.composer.field = ComposerField::Query;
        self.composer.status_lines.clear();
        self.pending_focus = None;
    }

    fn composer_matches(&self) -> Vec<&SessionRecord> {
        let query = self.composer.query.trim();
        self.ordered_sessions()
            .into_iter()
            .filter(|session| matches_query(session_switcher_text(session), query))
            .collect()
    }

    fn composer_rows(&self) -> Vec<ComposerRow> {
        let mut rows = vec![ComposerRow::Create];
        rows.extend(
            self.composer_matches()
                .into_iter()
                .map(|session| ComposerRow::Session(session.session_id.clone())),
        );
        rows
    }

    fn clamp_composer_selection(&mut self) {
        let len = self.composer_rows().len();
        if len == 0 {
            self.composer.selected = 0;
        } else {
            self.composer.selected = self.composer.selected.min(len.saturating_sub(1));
        }
    }

    fn on_resize(&mut self, width: u16, height: u16) {
        self.last_pane_size = Some((width, height));
    }

    fn drain_pty_messages(&mut self) {
        let Some(pty) = self.pty.as_mut() else {
            return;
        };
        loop {
            match pty.messages.try_recv() {
                Ok(message) => match message {
                    PtyEvent::Snapshot(bytes) => pty.replace(bytes),
                    PtyEvent::Output(bytes) => pty.push(bytes),
                    PtyEvent::Ended(summary) => {
                        pty.connected = false;
                        pty.last_error = Some(summary);
                    }
                    PtyEvent::Error(err) => {
                        pty.connected = false;
                        pty.last_error = Some(err);
                    }
                    PtyEvent::Closed => {
                        pty.connected = false;
                    }
                },
                Err(tokio::sync::mpsc::error::TryRecvError::Empty) => break,
                Err(tokio::sync::mpsc::error::TryRecvError::Disconnected) => {
                    pty.connected = false;
                    break;
                }
            }
        }
    }

    async fn handle_key(&mut self, key: KeyEvent) -> Result<bool> {
        if self.handle_modal_key(key).await? {
            return Ok(false);
        }

        if self.leader_pending {
            self.leader_pending = false;
            self.handle_leader_key(key).await?;
            return Ok(false);
        }

        if (key.code, key.modifiers) == LEADER_KEY {
            self.leader_pending = true;
            self.toast = Some("leader".to_string());
            return Ok(false);
        }

        if key.code == KeyCode::Char('c') && key.modifiers.contains(KeyModifiers::CONTROL) {
            return Ok(true);
        }

        if self.composer.active
            && matches!(self.focus, FocusTarget::Coordinator)
            && matches!(self.modal, Modal::None)
        {
            self.handle_composer_key(key).await?;
            return Ok(false);
        }

        if let Some(pty) = self.pty.as_mut()
            && matches!(self.focus, FocusTarget::Worker(_))
            && matches!(self.modal, Modal::None)
            && let Some(data) = encode_attach_key(key)
        {
            let _ = pty.commands.send(PtyCommand::Input(data));
            return Ok(false);
        }

        Ok(false)
    }

    async fn handle_leader_key(&mut self, key: KeyEvent) -> Result<()> {
        match key.code {
            KeyCode::Char('n') => self.run_command(Command::NextSession).await?,
            KeyCode::Char('p') => self.run_command(Command::PreviousSession).await?,
            KeyCode::Char('a') => self.run_command(Command::NextAttention).await?,
            KeyCode::Char('c') => self.run_command(Command::FocusCoordinator).await?,
            KeyCode::Char('s') => self.run_command(Command::SessionSwitcher).await?,
            KeyCode::Char('t') => self.run_command(Command::NewSession).await?,
            KeyCode::Char('w') => self.run_command(Command::WorktreeActions).await?,
            KeyCode::Char('g') => self.run_command(Command::GitStatus).await?,
            KeyCode::Char('d') => self.run_command(Command::Diff).await?,
            KeyCode::Char('x') => self.run_command(Command::StopSession).await?,
            KeyCode::Char(' ') => {
                self.modal = Modal::Palette;
                self.palette_query.clear();
                self.palette_selected = 0;
            }
            KeyCode::Esc => {
                self.toast = Some("leader canceled".to_string());
            }
            _ => {
                self.toast = Some("unknown leader binding".to_string());
            }
        }
        Ok(())
    }

    async fn handle_modal_key(&mut self, key: KeyEvent) -> Result<bool> {
        match self.modal {
            Modal::None => Ok(false),
            Modal::Palette => {
                self.handle_palette_key(key).await?;
                Ok(true)
            }
            Modal::WorktreeActions => {
                self.handle_worktree_modal_key(key);
                Ok(true)
            }
            Modal::GitStatus | Modal::Diff => {
                self.handle_detail_modal_key(key);
                Ok(true)
            }
            Modal::StopConfirm => {
                self.handle_stop_confirm_key(key).await?;
                Ok(true)
            }
        }
    }

    async fn run_command(&mut self, command: Command) -> Result<()> {
        match command {
            Command::NextSession => self.cycle_focus(1, false),
            Command::PreviousSession => self.cycle_focus(-1, false),
            Command::NextAttention => self.cycle_focus(1, true),
            Command::FocusCoordinator => self.focus = FocusTarget::Coordinator,
            Command::SessionSwitcher => self.open_composer(ComposerField::Query, false),
            Command::NewSession => self.open_composer(ComposerField::Query, true),
            Command::NewAgent => self.open_composer(ComposerField::Agent, true),
            Command::WorktreeActions => {
                self.open_review_actions();
            }
            Command::GitStatus => {
                self.open_git_status();
            }
            Command::Diff => {
                self.open_diff().await?;
            }
            Command::StopSession => {
                if self.focused_session().is_some() {
                    self.modal = Modal::StopConfirm;
                } else {
                    self.toast = Some("focus a worker session first".to_string());
                }
            }
        }
        Ok(())
    }

    fn open_git_status(&mut self) {
        let Some(session) = self.focused_session() else {
            self.toast = Some("focus a worker session first".to_string());
            return;
        };
        let sync = session.git_sync.as_str();
        let summary = session
            .git_status_summary
            .clone()
            .unwrap_or_else(|| "TODO: live git status sync not implemented yet".to_string());
        self.detail_text = format!(
            "repo      {}\nrepo_path  {}\nworktree   {}\nbranch     {}\nbase       {}\ngit_sync   {}\nconflicts  {}\n\n{}",
            session.repo_name,
            session.repo_path,
            session.worktree,
            session.branch,
            session.base_branch,
            sync,
            if session.has_conflicts { "yes" } else { "no" },
            summary,
        );
        self.detail_scroll = 0;
        self.modal = Modal::GitStatus;
    }

    async fn open_diff(&mut self) -> Result<()> {
        let Some(session_id) = self.focused_worker_session_id() else {
            self.toast = Some("focus a worker session first".to_string());
            return Ok(());
        };
        let response =
            send_request(&self.paths, &Request::DiffSession { session_id: session_id.to_string() })
                .await?;
        match response {
            Response::Diff { diff } => {
                self.diff_text = diff.diff;
                self.diff_scroll = 0;
                self.modal = Modal::Diff;
            }
            Response::Error { message } => self.toast = Some(message),
            other => bail!("unexpected response: {:?}", other),
        }
        Ok(())
    }

    async fn handle_palette_key(&mut self, key: KeyEvent) -> Result<()> {
        let items = filtered_palette_items(&self.palette_query);
        match key.code {
            KeyCode::Esc => self.close_modal(),
            KeyCode::Up => self.palette_selected = self.palette_selected.saturating_sub(1),
            KeyCode::Down => {
                if self.palette_selected + 1 < items.len() {
                    self.palette_selected += 1;
                }
            }
            KeyCode::Backspace => {
                self.palette_query.pop();
                self.palette_selected = 0;
            }
            KeyCode::Enter => {
                if let Some(item) = items.get(self.palette_selected) {
                    self.close_modal();
                    self.run_command(item.command).await?;
                }
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.palette_query.push(ch);
                self.palette_selected = 0;
            }
            _ => {}
        }
        Ok(())
    }

    async fn handle_composer_key(&mut self, key: KeyEvent) -> Result<()> {
        let row_count = self.composer_rows().len();
        match key.code {
            KeyCode::Esc => {
                if self.composer.query.is_empty() && self.composer.status_lines.is_empty() {
                    self.close_composer();
                } else {
                    self.composer.query.clear();
                    self.composer.status_lines.clear();
                    self.pending_focus = None;
                    self.clamp_composer_selection();
                }
            }
            KeyCode::Tab => {
                self.composer.field = match self.composer.field {
                    ComposerField::Query => ComposerField::Agent,
                    ComposerField::Agent => ComposerField::Query,
                };
            }
            KeyCode::Backspace => {
                self.composer.status_lines.clear();
                self.pending_focus = None;
                if self.composer.field == ComposerField::Agent {
                    self.composer.agent_input.pop();
                } else {
                    self.composer.query.pop();
                    self.clamp_composer_selection();
                }
            }
            KeyCode::Up => {
                self.composer.selected = self.composer.selected.saturating_sub(1);
            }
            KeyCode::Down => {
                if self.composer.selected + 1 < row_count {
                    self.composer.selected += 1;
                }
            }
            KeyCode::Enter => match self.composer_rows().get(self.composer.selected).cloned() {
                Some(ComposerRow::Session(session_id)) => {
                    self.focus = FocusTarget::Worker(session_id);
                    self.close_composer();
                }
                Some(ComposerRow::Create) | None => {
                    let title = self.composer.query.trim();
                    let agent = self.composer.agent_input.trim();
                    if agent.is_empty() {
                        self.toast = Some("agent cannot be empty".to_string());
                        return Ok(());
                    }
                    let workspace =
                        std::env::current_dir().context("failed to determine current directory")?;
                    let response = send_request(
                        &self.paths,
                        &Request::CreateSession {
                            workspace: workspace.to_string_lossy().to_string(),
                            title: (!title.is_empty()).then(|| title.to_string()),
                            agent: agent.to_string(),
                            model: if agent == "codex" {
                                Some(CODEX_MODELS[0].to_string())
                            } else {
                                None
                            },
                        },
                    )
                    .await?;
                    match response {
                        Response::CreateSession { session } => {
                            let created =
                                daemon_get_session(&self.paths, &session.session_id).await?;
                            self.composer.status_lines = vec![
                                format!("creating git worktree for {}", created.repo_name),
                                format!("base branch  {}", created.base_branch),
                                format!("new branch   {}", created.branch),
                                format!("worktree     {}", created.worktree),
                            ];
                            self.pending_focus = Some((
                                created.session_id.clone(),
                                std::time::Instant::now() + Duration::from_millis(1200),
                            ));
                            self.toast = Some(format!("created {}", created.title));
                        }
                        Response::Error { message } => self.toast = Some(message),
                        other => bail!("unexpected response: {:?}", other),
                    }
                }
            },
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.composer.status_lines.clear();
                self.pending_focus = None;
                if self.composer.field == ComposerField::Agent {
                    self.composer.agent_input.push(ch);
                } else {
                    self.composer.query.push(ch);
                    self.clamp_composer_selection();
                    if self.composer_rows().len() > 1 {
                        self.composer.selected = 1;
                    }
                }
            }
            _ => {}
        }
        Ok(())
    }

    fn handle_worktree_modal_key(&mut self, key: KeyEvent) {
        match key.code {
            KeyCode::Esc | KeyCode::Enter => self.close_modal(),
            _ => {}
        }
    }

    fn open_review_actions(&mut self) {
        let Some(session) = self.focused_session() else {
            self.toast = Some("focus a worker session first".to_string());
            return;
        };
        let summary =
            session.git_status_summary.clone().unwrap_or_else(|| "review ready".to_string());
        self.detail_text = format!(
            "session    {}\nrepo       {}\nbase       {}\nbranch     {}\nworktree   {}\nstatus     {}\nreview     {}\nconflicts  {}\n\nCLI actions\nagent diff {}\nagent accept {}\nagent discard {}\n\n`accept` will only apply when the repo checkout is clean and on `{}`. It performs a normal git merge and refuses to touch the upstream checkout if preflight predicts conflicts.",
            session.session_id,
            session.repo_name,
            session.base_branch,
            session.branch,
            session.worktree,
            session.status_string(),
            summary,
            if session.has_conflicts { "yes" } else { "no" },
            session.session_id,
            session.session_id,
            session.session_id,
            session.base_branch,
        );
        self.detail_scroll = 0;
        self.modal = Modal::WorktreeActions;
    }

    fn handle_detail_modal_key(&mut self, key: KeyEvent) {
        match key.code {
            KeyCode::Esc => self.close_modal(),
            KeyCode::Up => self.detail_scroll = self.detail_scroll.saturating_sub(1),
            KeyCode::Down => self.detail_scroll = self.detail_scroll.saturating_add(1),
            KeyCode::PageUp => self.detail_scroll = self.detail_scroll.saturating_sub(10),
            KeyCode::PageDown => self.detail_scroll = self.detail_scroll.saturating_add(10),
            _ => {}
        }
    }

    async fn handle_stop_confirm_key(&mut self, key: KeyEvent) -> Result<()> {
        match key.code {
            KeyCode::Esc => self.close_modal(),
            KeyCode::Enter => {
                if let Some(session_id) = self.focused_worker_session_id().map(str::to_string) {
                    kill_session(&self.paths, &session_id).await?;
                    self.toast = Some(format!("stopped {session_id}"));
                    self.focus = FocusTarget::Coordinator;
                }
                self.close_modal();
            }
            _ => {}
        }
        Ok(())
    }

    fn close_modal(&mut self) {
        self.modal = Modal::None;
        self.palette_query.clear();
        self.palette_selected = 0;
        self.detail_scroll = 0;
    }

    fn render(&mut self, frame: &mut Frame) {
        let area = frame.area();
        let root = Layout::default()
            .direction(Direction::Horizontal)
            .constraints([Constraint::Length(SIDEBAR_WIDTH), Constraint::Min(10)])
            .split(area);

        self.render_sidebar(frame, root[0]);
        self.render_main(frame, root[1]);
        self.render_overlay(frame, area);
    }

    fn render_sidebar(&self, frame: &mut Frame, area: Rect) {
        frame.render_widget(Block::default().borders(Borders::RIGHT), area);

        let ordered = self.ordered_sessions();
        let header = Paragraph::new(Line::from(vec![
            Span::styled("Sessions", Style::default().add_modifier(Modifier::BOLD)),
            Span::raw(format!(" ({})", self.sessions.len())),
        ]));
        frame.render_widget(header, Rect::new(area.x + 1, area.y, area.width.saturating_sub(2), 1));

        let available = area.height.saturating_sub(2);
        let visible_cards = (available / CARD_HEIGHT).max(1) as usize;
        let cards = ordered.into_iter().take(visible_cards.saturating_sub(1)).collect::<Vec<_>>();

        let mut y = area.y + 2;
        self.render_session_card(
            frame,
            Rect::new(area.x + 1, y, area.width.saturating_sub(2), CARD_HEIGHT.saturating_sub(1)),
            SidebarCard::coordinator(self.focus == FocusTarget::Coordinator),
        );
        y = y.saturating_add(CARD_HEIGHT);

        for session in cards {
            let is_focused =
                matches!(&self.focus, FocusTarget::Worker(id) if id == &session.session_id);
            let card = SidebarCard::from_session(session, is_focused);
            self.render_session_card(
                frame,
                Rect::new(
                    area.x + 1,
                    y,
                    area.width.saturating_sub(2),
                    CARD_HEIGHT.saturating_sub(1),
                ),
                card,
            );
            y = y.saturating_add(CARD_HEIGHT);
            if y >= area.bottom() {
                break;
            }
        }
    }

    fn render_session_card(&self, frame: &mut Frame, area: Rect, card: SidebarCard) {
        let border_style = if card.focused {
            Style::default().fg(Color::Cyan)
        } else {
            Style::default().fg(Color::DarkGray)
        };
        let block = Block::default().borders(Borders::ALL).border_style(border_style);
        let inner = block.inner(area);
        frame.render_widget(block, area);

        let lines = vec![
            Line::from(vec![
                Span::styled(
                    card.icon,
                    Style::default().fg(card.icon_color).add_modifier(Modifier::BOLD),
                ),
                Span::raw("  "),
                Span::styled(card.title, Style::default().add_modifier(Modifier::BOLD)),
            ]),
            Line::from(vec![Span::styled("repo    ", subtle_style()), Span::raw(card.repo_name)]),
            Line::from(vec![Span::styled("branch  ", subtle_style()), Span::raw(card.branch)]),
            Line::from(Span::styled(card.status_text, Style::default().fg(card.icon_color))),
        ];
        frame.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
    }

    fn render_main(&mut self, frame: &mut Frame, area: Rect) {
        match self.focused_session().cloned() {
            Some(session) => self.render_worker(frame, area, &session),
            None => self.render_coordinator(frame, area),
        }
    }

    fn render_coordinator(&self, frame: &mut Frame, area: Rect) {
        let block = Block::default().title("Coordinator").borders(Borders::ALL);
        let inner = block.inner(area);
        frame.render_widget(block, area);

        let composer_height = if self.composer.active { 12 } else { 0 };
        let sections = Layout::default()
            .direction(Direction::Vertical)
            .constraints([Constraint::Min(6), Constraint::Length(composer_height)])
            .split(inner);

        let ordered = self.ordered_sessions();
        let mut lines = vec![
            Line::from(vec![
                Span::styled("Attention queue", Style::default().add_modifier(Modifier::BOLD)),
                Span::raw("  "),
                Span::styled("Ctrl-b opens commands", subtle_style()),
            ]),
            Line::from(""),
        ];
        if ordered.is_empty() {
            lines.push(Line::from(Span::styled(
                "No worker sessions yet. Use Ctrl-b t to start one.",
                subtle_style(),
            )));
        } else {
            for session in ordered.iter().take(inner.height.saturating_sub(4) as usize) {
                lines.push(Line::from(vec![
                    Span::styled(session_icon(session), session_icon_style(session)),
                    Span::raw("  "),
                    Span::styled(
                        session.title.as_str(),
                        Style::default().add_modifier(Modifier::BOLD),
                    ),
                    Span::raw("  "),
                    Span::styled(session.repo_name.as_str(), subtle_style()),
                    Span::raw("  "),
                    Span::styled(session.branch.as_str(), subtle_style()),
                ]));
                lines.push(Line::from(Span::styled(session_status_text(session), subtle_style())));
            }
        }

        lines.push(Line::from(""));
        lines.push(Line::from(Span::styled(
            "TODO: richer coordinator summaries, cross-session review queues, and grouped worktree actions.",
            subtle_style(),
        )));

        frame.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), sections[0]);
        if self.composer.active {
            self.render_inline_composer(frame, sections[1]);
        }
    }

    fn render_worker(&mut self, frame: &mut Frame, area: Rect, session: &SessionRecord) {
        let block = Block::default()
            .title(format!("{}  {}", session.repo_name, session.branch))
            .borders(Borders::ALL);
        let inner = block.inner(area);
        frame.render_widget(block, area);

        let sections = Layout::default()
            .direction(Direction::Vertical)
            .constraints([Constraint::Length(5), Constraint::Min(4)])
            .split(inner);

        let summary = vec![
            Line::from(vec![
                Span::styled(
                    session_icon(session),
                    session_icon_style(session).add_modifier(Modifier::BOLD),
                ),
                Span::raw("  "),
                Span::styled(session.title.as_str(), Style::default().add_modifier(Modifier::BOLD)),
            ]),
            Line::from(vec![
                Span::styled("repo      ", subtle_style()),
                Span::raw(session.repo_path.as_str()),
            ]),
            Line::from(vec![
                Span::styled("worktree  ", subtle_style()),
                Span::raw(session.worktree.as_str()),
            ]),
            Line::from(vec![
                Span::styled("status    ", subtle_style()),
                Span::raw(session_status_text(session)),
                Span::raw("  "),
                Span::styled("git ", subtle_style()),
                Span::raw(session.git_sync.as_str()),
            ]),
        ];
        frame.render_widget(Paragraph::new(summary).wrap(Wrap { trim: false }), sections[0]);

        let pty_area = sections[1];
        let pane = Block::default().borders(Borders::TOP).title("PTY");
        let pty_inner = pane.inner(pty_area);
        frame.render_widget(pane, pty_area);

        if let Some(pty) = &mut self.pty
            && pty.session_id == session.session_id
        {
            let size = (pty_inner.width.max(1), pty_inner.height.max(1));
            if self.last_pane_size != Some(size) {
                self.last_pane_size = Some(size);
                pty.resize(size.0, size.1);
                let _ = pty.commands.send(PtyCommand::Resize { cols: size.0, rows: size.1 });
            }
        }

        if let Some(pty) = &self.pty
            && pty.session_id == session.session_id
        {
            frame.render_widget(
                Paragraph::new(pty.rendered_lines())
                    .wrap(Wrap { trim: false })
                    .block(Block::default()),
                pty_inner,
            );
        } else {
            frame.render_widget(
                Paragraph::new(self.focused_history.as_str())
                    .wrap(Wrap { trim: false })
                    .block(Block::default()),
                pty_inner,
            );
        }
    }

    fn render_overlay(&self, frame: &mut Frame, area: Rect) {
        match self.modal {
            Modal::None => {}
            Modal::Palette => self.render_palette(frame, area),
            Modal::WorktreeActions => self.render_detail_modal(
                frame,
                area,
                "Worktree Actions",
                &self.detail_text,
                self.detail_scroll,
            ),
            Modal::GitStatus => self.render_detail_modal(
                frame,
                area,
                "Git Status",
                &self.detail_text,
                self.detail_scroll,
            ),
            Modal::Diff => {
                self.render_detail_modal(frame, area, "Diff", &self.diff_text, self.diff_scroll)
            }
            Modal::StopConfirm => self.render_stop_confirm(frame, area),
        }

        if self.leader_pending {
            let overlay = centered_rect(28, 10, area);
            frame.render_widget(WidgetClear, overlay);
            frame.render_widget(
                Paragraph::new("leader: n/p/a/c/s/t/w/g/d/x")
                    .block(Block::default().borders(Borders::ALL).title("Prefix")),
                overlay,
            );
        } else if let Some(message) = &self.toast {
            let overlay = centered_rect(40, 10, area);
            frame.render_widget(WidgetClear, overlay);
            frame.render_widget(
                Paragraph::new(message.as_str())
                    .block(Block::default().borders(Borders::ALL).title("Notice")),
                overlay,
            );
        }
    }

    fn render_palette(&self, frame: &mut Frame, area: Rect) {
        let overlay = centered_rect(56, 44, area);
        let items = filtered_palette_items(&self.palette_query);
        let mut lines = vec![Line::from(vec![
            Span::styled("> ", Style::default().fg(Color::Cyan)),
            Span::raw(self.palette_query.as_str()),
        ])];
        lines.push(Line::from(""));
        for (index, item) in items.iter().take(9).enumerate() {
            let style = if index == self.palette_selected {
                Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
            } else {
                Style::default()
            };
            lines.push(Line::from(vec![
                Span::styled(format!("{:>2}", item.key_hint), subtle_style()),
                Span::raw("  "),
                Span::styled(item.title, style),
            ]));
        }
        frame.render_widget(WidgetClear, overlay);
        frame.render_widget(
            Paragraph::new(lines)
                .block(Block::default().borders(Borders::ALL).title("Command Palette"))
                .wrap(Wrap { trim: false }),
            overlay,
        );
    }

    fn render_inline_composer(&self, frame: &mut Frame, area: Rect) {
        let block = Block::default().borders(Borders::TOP).title("Compose");
        let inner = block.inner(area);
        frame.render_widget(block, area);

        let mut lines = vec![
            Line::from(vec![
                Span::styled(
                    if self.composer.field == ComposerField::Query { "> " } else { "  " },
                    Style::default().fg(Color::Cyan),
                ),
                Span::styled("query  ", subtle_style()),
                Span::raw(self.composer.query.as_str()),
            ]),
            Line::from(vec![
                Span::styled(
                    if self.composer.field == ComposerField::Agent { "> " } else { "  " },
                    Style::default().fg(Color::Cyan),
                ),
                Span::styled("agent  ", subtle_style()),
                Span::raw(self.composer.agent_input.as_str()),
            ]),
            Line::from(Span::styled(
                "Type to filter sessions or name a new session. Enter opens the selected row. Tab switches field.",
                subtle_style(),
            )),
            Line::from(""),
        ];

        for (index, row) in self.composer_rows().into_iter().take(5).enumerate() {
            let style = if index == self.composer.selected {
                Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
            } else {
                Style::default()
            };
            match row {
                ComposerRow::Create => {
                    let label = if self.composer.query.trim().is_empty() {
                        "Create new session".to_string()
                    } else {
                        format!("Create new session: {}", self.composer.query.trim())
                    };
                    lines.push(Line::from(vec![
                        Span::styled("+", Style::default().fg(Color::Green)),
                        Span::raw("  "),
                        Span::styled(label, style),
                    ]));
                }
                ComposerRow::Session(session_id) => {
                    if let Some(session) =
                        self.sessions.iter().find(|item| item.session_id == session_id)
                    {
                        lines.push(Line::from(vec![
                            Span::styled(session_icon(session), session_icon_style(session)),
                            Span::raw("  "),
                            Span::styled(session_elapsed_label(session), style),
                            Span::raw("  "),
                            Span::styled(session.title.as_str(), style),
                            Span::raw("  "),
                            Span::styled(session.repo_name.as_str(), subtle_style()),
                            Span::raw("  "),
                            Span::styled(session.branch.as_str(), subtle_style()),
                        ]));
                    }
                }
            }
        }

        if !self.composer.status_lines.is_empty() {
            lines.push(Line::from(""));
            for line in &self.composer.status_lines {
                lines.push(Line::from(Span::styled(line.as_str(), subtle_style())));
            }
        }

        frame.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
    }

    fn render_detail_modal(
        &self,
        frame: &mut Frame,
        area: Rect,
        title: &str,
        body: &str,
        scroll: u16,
    ) {
        let overlay = centered_rect(74, 70, area);
        frame.render_widget(WidgetClear, overlay);
        frame.render_widget(
            Paragraph::new(body)
                .scroll((scroll, 0))
                .wrap(Wrap { trim: false })
                .block(Block::default().borders(Borders::ALL).title(title)),
            overlay,
        );
    }

    fn render_stop_confirm(&self, frame: &mut Frame, area: Rect) {
        let overlay = centered_rect(40, 20, area);
        let body = self
            .focused_worker_session_id()
            .map(|session_id| format!("Stop session {session_id}?\n\nEnter confirms. Esc cancels."))
            .unwrap_or_else(|| "No session selected.".to_string());
        frame.render_widget(WidgetClear, overlay);
        frame.render_widget(
            Paragraph::new(body)
                .block(Block::default().borders(Borders::ALL).title("Stop Session")),
            overlay,
        );
    }
}

struct FocusedPty {
    session_id: String,
    commands: UnboundedSender<PtyCommand>,
    messages: UnboundedReceiver<PtyEvent>,
    terminal: TerminalSurface,
    connected: bool,
    last_error: Option<String>,
}

impl FocusedPty {
    fn spawn(paths: AppPaths, session_id: String) -> Self {
        let (command_tx, command_rx) = unbounded_channel();
        let (message_tx, message_rx) = unbounded_channel();
        tokio::spawn(run_pty_session(paths, session_id.clone(), command_rx, message_tx));
        Self {
            session_id,
            commands: command_tx,
            messages: message_rx,
            terminal: TerminalSurface::new(80, 24),
            connected: true,
            last_error: None,
        }
    }

    fn replace(&mut self, bytes: Vec<u8>) {
        self.terminal.reset();
        self.terminal.process(&bytes);
    }

    fn push(&mut self, bytes: Vec<u8>) {
        self.terminal.process(&bytes);
    }

    fn resize(&mut self, cols: u16, rows: u16) {
        self.terminal.resize(cols.max(1), rows.max(1));
    }

    fn rendered_lines(&self) -> Vec<Line<'static>> {
        let mut lines = self.terminal.render_lines(self.connected);
        if let Some(error) = &self.last_error {
            if !lines.is_empty() {
                lines.push(Line::from(""));
            }
            lines.push(Line::from(Span::styled(error.clone(), Style::default().fg(Color::Red))));
        }
        lines
    }
}

enum PtyCommand {
    Input(Vec<u8>),
    Resize { cols: u16, rows: u16 },
    Close,
}

enum PtyEvent {
    Snapshot(Vec<u8>),
    Output(Vec<u8>),
    Ended(String),
    Error(String),
    Closed,
}

async fn run_pty_session(
    paths: AppPaths,
    session_id: String,
    mut commands: UnboundedReceiver<PtyCommand>,
    messages: UnboundedSender<PtyEvent>,
) {
    let result = async {
        let mut stream = UnixStream::connect(paths.socket.as_std_path())
            .await
            .with_context(|| format!("failed to connect to {}", paths.socket))?;
        write_request(
            &mut stream,
            &Request::AttachSession {
                session_id: session_id.clone(),
                kind: AttachmentKind::Tui,
            },
        )
        .await?;

        let (read_half, mut write_half) = stream.into_split();
        let mut reader = BufReader::new(read_half);
        let Some(initial) = read_response(&mut reader).await? else {
            bail!("agentd closed the connection");
        };

        match initial {
            Response::Attached { snapshot, .. } => {
                let _ = messages.send(PtyEvent::Snapshot(snapshot));
            }
            Response::SessionEnded { status, .. } => {
                let _ = messages.send(PtyEvent::Ended(format!("session ended: {}", status_label(status))));
                return Ok::<(), anyhow::Error>(());
            }
            Response::Error { message } => {
                let _ = messages.send(PtyEvent::Error(message));
                return Ok(());
            }
            other => bail!("unexpected response: {:?}", other),
        }

        loop {
            tokio::select! {
                maybe_command = commands.recv() => {
                    match maybe_command {
                        Some(PtyCommand::Input(data)) => {
                            write_request(&mut write_half, &Request::AttachInput { data }).await?;
                        }
                        Some(PtyCommand::Resize { cols, rows }) => {
                            write_request(&mut write_half, &Request::AttachResize { cols, rows }).await?;
                        }
                        Some(PtyCommand::Close) | None => {
                            break;
                        }
                    }
                }
                maybe_response = read_response(&mut reader) => {
                    let Some(response) = maybe_response? else {
                        break;
                    };
                    match response {
                        Response::PtyOutput { data } => {
                            let _ = messages.send(PtyEvent::Output(data));
                        }
                        Response::SessionEnded { status, error, .. } => {
                            let summary = error.unwrap_or_else(|| format!("session ended: {}", status_label(status)));
                            let _ = messages.send(PtyEvent::Ended(summary));
                            break;
                        }
                        Response::EndOfStream => break,
                        Response::Error { message } => {
                            let _ = messages.send(PtyEvent::Error(message));
                            break;
                        }
                        other => {
                            let _ = messages.send(PtyEvent::Error(format!("unexpected response: {other:?}")));
                            break;
                        }
                    }
                }
            }
        }

        Ok(())
    }
    .await;

    if let Err(err) = result {
        let _ = messages.send(PtyEvent::Error(err.to_string()));
    }
    let _ = messages.send(PtyEvent::Closed);
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct TerminalCellStyle {
    fg: Option<Color>,
    bg: Option<Color>,
    bold: bool,
    underlined: bool,
    reversed: bool,
}

#[derive(Clone, Debug)]
struct TerminalCell {
    ch: char,
    style: TerminalCellStyle,
}

impl Default for TerminalCell {
    fn default() -> Self {
        Self { ch: ' ', style: TerminalCellStyle::default() }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum TerminalParserState {
    Ground,
    Escape,
    Csi,
    Osc,
    OscEscape,
}

struct TerminalSurface {
    cols: u16,
    rows: u16,
    cursor_col: usize,
    cursor_row: usize,
    saved_cursor: (usize, usize),
    style: TerminalCellStyle,
    cells: Vec<TerminalCell>,
    parser_state: TerminalParserState,
    csi_buffer: String,
}

impl TerminalSurface {
    fn new(cols: u16, rows: u16) -> Self {
        let cols = cols.max(1);
        let rows = rows.max(1);
        Self {
            cols,
            rows,
            cursor_col: 0,
            cursor_row: 0,
            saved_cursor: (0, 0),
            style: TerminalCellStyle::default(),
            cells: vec![TerminalCell::default(); cols as usize * rows as usize],
            parser_state: TerminalParserState::Ground,
            csi_buffer: String::new(),
        }
    }

    fn reset(&mut self) {
        let cols = self.cols;
        let rows = self.rows;
        *self = Self::new(cols, rows);
    }

    fn resize(&mut self, cols: u16, rows: u16) {
        let cols = cols.max(1);
        let rows = rows.max(1);
        if self.cols == cols && self.rows == rows {
            return;
        }

        let old_cols = self.cols as usize;
        let old_rows = self.rows as usize;
        let mut cells = vec![TerminalCell::default(); cols as usize * rows as usize];
        let copy_rows = old_rows.min(rows as usize);
        let copy_cols = old_cols.min(cols as usize);
        for row in 0..copy_rows {
            let old_start = row * old_cols;
            let new_start = row * cols as usize;
            cells[new_start..new_start + copy_cols]
                .clone_from_slice(&self.cells[old_start..old_start + copy_cols]);
        }

        self.cols = cols;
        self.rows = rows;
        self.cells = cells;
        self.cursor_col = self.cursor_col.min(cols.saturating_sub(1) as usize);
        self.cursor_row = self.cursor_row.min(rows.saturating_sub(1) as usize);
    }

    fn process(&mut self, bytes: &[u8]) {
        for ch in String::from_utf8_lossy(bytes).chars() {
            match self.parser_state {
                TerminalParserState::Ground => self.process_ground(ch),
                TerminalParserState::Escape => self.process_escape(ch),
                TerminalParserState::Csi => self.process_csi(ch),
                TerminalParserState::Osc => self.process_osc(ch),
                TerminalParserState::OscEscape => self.process_osc_escape(ch),
            }
        }
    }

    fn render_lines(&self, show_cursor: bool) -> Vec<Line<'static>> {
        let mut lines = Vec::with_capacity(self.rows as usize);
        for row in 0..self.rows as usize {
            let mut spans = Vec::new();
            let mut current_style = None::<Style>;
            let mut current_text = String::new();
            for col in 0..self.cols as usize {
                let mut cell = self.cells[self.index(row, col)].clone();
                if show_cursor && row == self.cursor_row && col == self.cursor_col {
                    cell.style.reversed = !cell.style.reversed;
                }
                let style = terminal_style(cell.style);
                if current_style == Some(style) {
                    current_text.push(cell.ch);
                } else {
                    if !current_text.is_empty() {
                        spans.push(Span::styled(
                            std::mem::take(&mut current_text),
                            current_style.unwrap_or_default(),
                        ));
                    }
                    current_style = Some(style);
                    current_text.push(cell.ch);
                }
            }
            if !current_text.is_empty() {
                spans.push(Span::styled(current_text, current_style.unwrap_or_default()));
            }
            lines.push(Line::from(spans));
        }
        lines
    }

    fn process_ground(&mut self, ch: char) {
        match ch {
            '\u{1b}' => self.parser_state = TerminalParserState::Escape,
            '\n' => self.newline(),
            '\r' => self.cursor_col = 0,
            '\u{8}' => self.cursor_col = self.cursor_col.saturating_sub(1),
            '\t' => {
                let next_tab = ((self.cursor_col / 8) + 1) * 8;
                while self.cursor_col < next_tab {
                    self.put_char(' ');
                }
            }
            ch if ch.is_control() => {}
            ch => self.put_char(ch),
        }
    }

    fn process_escape(&mut self, ch: char) {
        match ch {
            '[' => {
                self.csi_buffer.clear();
                self.parser_state = TerminalParserState::Csi;
            }
            ']' => self.parser_state = TerminalParserState::Osc,
            '7' => {
                self.saved_cursor = (self.cursor_col, self.cursor_row);
                self.parser_state = TerminalParserState::Ground;
            }
            '8' => {
                (self.cursor_col, self.cursor_row) = self.saved_cursor;
                self.clamp_cursor();
                self.parser_state = TerminalParserState::Ground;
            }
            'c' => {
                self.reset();
                self.parser_state = TerminalParserState::Ground;
            }
            'D' => {
                self.index_line();
                self.parser_state = TerminalParserState::Ground;
            }
            'E' => {
                self.index_line();
                self.cursor_col = 0;
                self.parser_state = TerminalParserState::Ground;
            }
            'M' => {
                self.reverse_index();
                self.parser_state = TerminalParserState::Ground;
            }
            _ => self.parser_state = TerminalParserState::Ground,
        }
    }

    fn process_csi(&mut self, ch: char) {
        if ('@'..='~').contains(&ch) {
            let buffer = std::mem::take(&mut self.csi_buffer);
            self.dispatch_csi(&buffer, ch);
            self.parser_state = TerminalParserState::Ground;
        } else {
            self.csi_buffer.push(ch);
        }
    }

    fn process_osc(&mut self, ch: char) {
        match ch {
            '\u{7}' => self.parser_state = TerminalParserState::Ground,
            '\u{1b}' => self.parser_state = TerminalParserState::OscEscape,
            _ => {}
        }
    }

    fn process_osc_escape(&mut self, ch: char) {
        self.parser_state =
            if ch == '\\' { TerminalParserState::Ground } else { TerminalParserState::Osc };
    }

    fn dispatch_csi(&mut self, params: &str, action: char) {
        let private = params.starts_with('?');
        let params = if private { &params[1..] } else { params };
        let parsed = parse_csi_params(params);
        match action {
            'A' => {
                self.cursor_row = self.cursor_row.saturating_sub(first_param(&parsed, 1) as usize)
            }
            'B' => {
                self.cursor_row = (self.cursor_row + first_param(&parsed, 1) as usize)
                    .min(self.rows.saturating_sub(1) as usize)
            }
            'C' => {
                self.cursor_col = (self.cursor_col + first_param(&parsed, 1) as usize)
                    .min(self.cols.saturating_sub(1) as usize)
            }
            'D' => {
                self.cursor_col = self.cursor_col.saturating_sub(first_param(&parsed, 1) as usize)
            }
            'G' => self.cursor_col = first_param(&parsed, 1).saturating_sub(1) as usize,
            'H' | 'f' => {
                self.cursor_row = first_param(&parsed, 1).saturating_sub(1) as usize;
                self.cursor_col = nth_param(&parsed, 1, 1).saturating_sub(1) as usize;
                self.clamp_cursor();
            }
            'J' => self.clear_screen(first_param(&parsed, 0)),
            'K' => self.clear_line(first_param(&parsed, 0)),
            'L' => self.insert_lines(first_param(&parsed, 1) as usize),
            'M' => self.delete_lines(first_param(&parsed, 1) as usize),
            'P' => self.delete_chars(first_param(&parsed, 1) as usize),
            'X' => self.erase_chars(first_param(&parsed, 1) as usize),
            '@' => self.insert_blank_chars(first_param(&parsed, 1) as usize),
            'd' => {
                self.cursor_row = first_param(&parsed, 1).saturating_sub(1) as usize;
                self.clamp_cursor();
            }
            'm' => self.apply_sgr(&parsed),
            's' => self.saved_cursor = (self.cursor_col, self.cursor_row),
            'u' => {
                (self.cursor_col, self.cursor_row) = self.saved_cursor;
                self.clamp_cursor();
            }
            'h' | 'l' if private => {
                if parsed.first().copied().flatten() == Some(1049) {
                    self.clear_all();
                    self.cursor_col = 0;
                    self.cursor_row = 0;
                }
            }
            _ => {}
        }
    }

    fn apply_sgr(&mut self, params: &[Option<u16>]) {
        if params.is_empty() {
            self.style = TerminalCellStyle::default();
            return;
        }

        let mut index = 0;
        while index < params.len() {
            let value = params[index].unwrap_or(0);
            match value {
                0 => self.style = TerminalCellStyle::default(),
                1 => self.style.bold = true,
                4 => self.style.underlined = true,
                7 => self.style.reversed = true,
                22 => self.style.bold = false,
                24 => self.style.underlined = false,
                27 => self.style.reversed = false,
                30..=37 => self.style.fg = Some(ansi_color(value - 30, false)),
                39 => self.style.fg = None,
                40..=47 => self.style.bg = Some(ansi_color(value - 40, false)),
                49 => self.style.bg = None,
                90..=97 => self.style.fg = Some(ansi_color(value - 90, true)),
                100..=107 => self.style.bg = Some(ansi_color(value - 100, true)),
                38 | 48 => {
                    let is_fg = value == 38;
                    if let Some((color, consumed)) = parse_extended_color(params, index + 1) {
                        if is_fg {
                            self.style.fg = Some(color);
                        } else {
                            self.style.bg = Some(color);
                        }
                        index += consumed;
                    }
                }
                _ => {}
            }
            index += 1;
        }
    }

    fn put_char(&mut self, ch: char) {
        if self.cursor_row >= self.rows as usize || self.cursor_col >= self.cols as usize {
            self.clamp_cursor();
        }
        let index = self.index(self.cursor_row, self.cursor_col);
        self.cells[index] = TerminalCell { ch, style: self.style };
        if self.cursor_col + 1 >= self.cols as usize {
            self.cursor_col = 0;
            self.index_line();
        } else {
            self.cursor_col += 1;
        }
    }

    fn index_line(&mut self) {
        if self.cursor_row + 1 >= self.rows as usize {
            self.scroll_up(1);
        } else {
            self.cursor_row += 1;
        }
    }

    fn newline(&mut self) {
        self.index_line();
    }

    fn reverse_index(&mut self) {
        if self.cursor_row == 0 {
            self.scroll_down(1);
        } else {
            self.cursor_row -= 1;
        }
    }

    fn clear_screen(&mut self, mode: u16) {
        match mode {
            0 => {
                self.clear_line(0);
                for row in self.cursor_row + 1..self.rows as usize {
                    self.clear_row(row);
                }
            }
            1 => {
                for row in 0..self.cursor_row {
                    self.clear_row(row);
                }
                self.clear_line(1);
            }
            _ => self.clear_all(),
        }
    }

    fn clear_line(&mut self, mode: u16) {
        let row = self.cursor_row;
        match mode {
            0 => {
                for col in self.cursor_col..self.cols as usize {
                    let index = self.index(row, col);
                    self.cells[index] = TerminalCell::default();
                }
            }
            1 => {
                for col in 0..=self.cursor_col.min(self.cols.saturating_sub(1) as usize) {
                    let index = self.index(row, col);
                    self.cells[index] = TerminalCell::default();
                }
            }
            _ => self.clear_row(row),
        }
    }

    fn insert_blank_chars(&mut self, count: usize) {
        let row = self.cursor_row;
        let width = self.cols as usize;
        let count = count.min(width.saturating_sub(self.cursor_col));
        if count == 0 {
            return;
        }
        for col in (self.cursor_col..width).rev() {
            let target = col + count;
            if target < width {
                let src = self.index(row, col);
                let dst = self.index(row, target);
                self.cells[dst] = self.cells[src].clone();
            }
        }
        for col in self.cursor_col..(self.cursor_col + count).min(width) {
            let index = self.index(row, col);
            self.cells[index] = TerminalCell::default();
        }
    }

    fn delete_chars(&mut self, count: usize) {
        let row = self.cursor_row;
        let width = self.cols as usize;
        let count = count.min(width.saturating_sub(self.cursor_col));
        for col in self.cursor_col..width {
            let src = col + count;
            let dst = self.index(row, col);
            self.cells[dst] = if src < width {
                self.cells[self.index(row, src)].clone()
            } else {
                TerminalCell::default()
            };
        }
    }

    fn erase_chars(&mut self, count: usize) {
        for col in self.cursor_col..(self.cursor_col + count).min(self.cols as usize) {
            let index = self.index(self.cursor_row, col);
            self.cells[index] = TerminalCell::default();
        }
    }

    fn insert_lines(&mut self, count: usize) {
        let row = self.cursor_row;
        let width = self.cols as usize;
        let count = count.min(self.rows as usize - row);
        for target_row in (row..self.rows as usize).rev() {
            let src_row = target_row.saturating_sub(count);
            for col in 0..width {
                let dst = self.index(target_row, col);
                self.cells[dst] = if src_row >= row {
                    self.cells[self.index(src_row, col)].clone()
                } else {
                    TerminalCell::default()
                };
            }
        }
    }

    fn delete_lines(&mut self, count: usize) {
        let row = self.cursor_row;
        let width = self.cols as usize;
        let count = count.min(self.rows as usize - row);
        for target_row in row..self.rows as usize {
            let src_row = target_row + count;
            for col in 0..width {
                let dst = self.index(target_row, col);
                self.cells[dst] = if src_row < self.rows as usize {
                    self.cells[self.index(src_row, col)].clone()
                } else {
                    TerminalCell::default()
                };
            }
        }
    }

    fn clear_row(&mut self, row: usize) {
        for col in 0..self.cols as usize {
            let index = self.index(row, col);
            self.cells[index] = TerminalCell::default();
        }
    }

    fn clear_all(&mut self) {
        for cell in &mut self.cells {
            *cell = TerminalCell::default();
        }
    }

    fn scroll_up(&mut self, count: usize) {
        let width = self.cols as usize;
        let count = count.min(self.rows as usize);
        if count == 0 {
            return;
        }
        for row in 0..self.rows as usize {
            for col in 0..width {
                let dst = self.index(row, col);
                self.cells[dst] = if row + count < self.rows as usize {
                    self.cells[self.index(row + count, col)].clone()
                } else {
                    TerminalCell::default()
                };
            }
        }
    }

    fn scroll_down(&mut self, count: usize) {
        let width = self.cols as usize;
        let count = count.min(self.rows as usize);
        if count == 0 {
            return;
        }
        for row in (0..self.rows as usize).rev() {
            for col in 0..width {
                let dst = self.index(row, col);
                self.cells[dst] = if row >= count {
                    self.cells[self.index(row - count, col)].clone()
                } else {
                    TerminalCell::default()
                };
            }
        }
    }

    fn clamp_cursor(&mut self) {
        self.cursor_col = self.cursor_col.min(self.cols.saturating_sub(1) as usize);
        self.cursor_row = self.cursor_row.min(self.rows.saturating_sub(1) as usize);
    }

    fn index(&self, row: usize, col: usize) -> usize {
        row * self.cols as usize + col
    }
}

fn parse_csi_params(params: &str) -> Vec<Option<u16>> {
    if params.is_empty() {
        return Vec::new();
    }
    params
        .split(';')
        .map(|part| if part.is_empty() { None } else { part.parse::<u16>().ok() })
        .collect()
}

fn first_param(params: &[Option<u16>], default: u16) -> u16 {
    params.first().copied().flatten().unwrap_or(default)
}

fn nth_param(params: &[Option<u16>], index: usize, default: u16) -> u16 {
    params.get(index).copied().flatten().unwrap_or(default)
}

fn parse_extended_color(params: &[Option<u16>], start: usize) -> Option<(Color, usize)> {
    match params.get(start).copied().flatten()? {
        5 => {
            let value = params.get(start + 1).copied().flatten()?;
            Some((ansi_256_color(value), 2))
        }
        2 => {
            let r = params.get(start + 1).copied().flatten()?;
            let g = params.get(start + 2).copied().flatten()?;
            let b = params.get(start + 3).copied().flatten()?;
            Some((Color::Rgb(r as u8, g as u8, b as u8), 4))
        }
        _ => None,
    }
}

fn ansi_color(index: u16, bright: bool) -> Color {
    match (index, bright) {
        (0, false) => Color::Black,
        (1, false) => Color::Red,
        (2, false) => Color::Green,
        (3, false) => Color::Yellow,
        (4, false) => Color::Blue,
        (5, false) => Color::Magenta,
        (6, false) => Color::Cyan,
        (7, false) => Color::Gray,
        (0, true) => Color::DarkGray,
        (1, true) => Color::LightRed,
        (2, true) => Color::LightGreen,
        (3, true) => Color::LightYellow,
        (4, true) => Color::LightBlue,
        (5, true) => Color::LightMagenta,
        (6, true) => Color::LightCyan,
        _ => Color::White,
    }
}

fn ansi_256_color(index: u16) -> Color {
    match index {
        0..=15 => ansi_color(index % 8, index >= 8),
        16..=231 => {
            let value = index - 16;
            let r = value / 36;
            let g = (value / 6) % 6;
            let b = value % 6;
            let convert = |component: u16| -> u8 {
                if component == 0 { 0 } else { (component * 40 + 55) as u8 }
            };
            Color::Rgb(convert(r), convert(g), convert(b))
        }
        232..=255 => {
            let shade = ((index - 232) * 10 + 8) as u8;
            Color::Rgb(shade, shade, shade)
        }
        _ => Color::White,
    }
}

fn terminal_style(cell: TerminalCellStyle) -> Style {
    let mut style = Style::default();
    let (fg, bg) = if cell.reversed { (cell.bg, cell.fg) } else { (cell.fg, cell.bg) };
    if let Some(fg) = fg {
        style = style.fg(fg);
    }
    if let Some(bg) = bg {
        style = style.bg(bg);
    }
    if cell.bold {
        style = style.add_modifier(Modifier::BOLD);
    }
    if cell.underlined {
        style = style.add_modifier(Modifier::UNDERLINED);
    }
    style
}

struct SidebarCard {
    icon: &'static str,
    icon_color: Color,
    title: String,
    repo_name: String,
    branch: String,
    status_text: String,
    focused: bool,
}

impl SidebarCard {
    fn coordinator(focused: bool) -> Self {
        Self {
            icon: "C",
            icon_color: Color::Cyan,
            title: "coordinator".to_string(),
            repo_name: "all repos".to_string(),
            branch: "overview".to_string(),
            status_text: "attention queue".to_string(),
            focused,
        }
    }

    fn from_session(session: &SessionRecord, focused: bool) -> Self {
        Self {
            icon: session_icon(session),
            icon_color: session_icon_color(session),
            title: session.title.clone(),
            repo_name: session.repo_name.clone(),
            branch: session.branch.clone(),
            status_text: session_status_text(session),
            focused,
        }
    }
}

fn filtered_palette_items(query: &str) -> Vec<PaletteItem> {
    palette_items()
        .into_iter()
        .filter(|item| matches_query(format!("{} {}", item.key_hint, item.title), query))
        .collect()
}

fn palette_items() -> Vec<PaletteItem> {
    vec![
        PaletteItem { key_hint: "n", title: "Next Session", command: Command::NextSession },
        PaletteItem { key_hint: "p", title: "Previous Session", command: Command::PreviousSession },
        PaletteItem { key_hint: "a", title: "Next Attention", command: Command::NextAttention },
        PaletteItem {
            key_hint: "c",
            title: "Focus Coordinator",
            command: Command::FocusCoordinator,
        },
        PaletteItem { key_hint: "s", title: "Session Switcher", command: Command::SessionSwitcher },
        PaletteItem { key_hint: "t", title: "New Session", command: Command::NewSession },
        PaletteItem { key_hint: "N", title: "New Agent", command: Command::NewAgent },
        PaletteItem { key_hint: "w", title: "Review Actions", command: Command::WorktreeActions },
        PaletteItem { key_hint: "g", title: "Git Status", command: Command::GitStatus },
        PaletteItem { key_hint: "d", title: "Diff", command: Command::Diff },
        PaletteItem { key_hint: "x", title: "Stop Session", command: Command::StopSession },
    ]
}

fn session_rank(session: &SessionRecord) -> u8 {
    if session.status == SessionStatus::NeedsInput {
        0
    } else if matches!(session.status, SessionStatus::Failed | SessionStatus::UnknownRecovered)
        || session.has_conflicts
    {
        1
    } else if session.integration_state == IntegrationState::PendingReview {
        2
    } else if matches!(session.status, SessionStatus::Running | SessionStatus::Creating) {
        3
    } else if session.status == SessionStatus::Paused {
        4
    } else {
        5
    }
}

fn session_icon(session: &SessionRecord) -> &'static str {
    if session.integration_state == IntegrationState::PendingReview {
        "⧖"
    } else {
        match session.status {
            SessionStatus::NeedsInput => "◦",
            SessionStatus::Failed => "✖",
            SessionStatus::UnknownRecovered => "⚠",
            SessionStatus::Running | SessionStatus::Creating => "●",
            SessionStatus::Paused => "⏸",
            SessionStatus::Exited => "✔",
        }
    }
}

fn session_icon_color(session: &SessionRecord) -> Color {
    if session.integration_state == IntegrationState::PendingReview {
        Color::Blue
    } else {
        match session.status {
            SessionStatus::NeedsInput => Color::Yellow,
            SessionStatus::Failed => Color::Red,
            SessionStatus::UnknownRecovered => Color::Yellow,
            SessionStatus::Running | SessionStatus::Creating => Color::Green,
            SessionStatus::Paused => Color::DarkGray,
            SessionStatus::Exited => Color::Green,
        }
    }
}

fn session_icon_style(session: &SessionRecord) -> Style {
    let mut style = Style::default().fg(session_icon_color(session));
    if matches!(session.status, SessionStatus::NeedsInput | SessionStatus::Paused) {
        style = style.add_modifier(Modifier::DIM);
    }
    style
}

fn session_status_text(session: &SessionRecord) -> String {
    if let Some(summary) = &session.attention_summary {
        return summary.clone();
    }
    if session.has_conflicts {
        return "conflicts detected".to_string();
    }
    if let Some(summary) = &session.git_status_summary {
        return summary.clone();
    }
    if session.integration_state == IntegrationState::PendingReview {
        return "review ready".to_string();
    }
    match session.status {
        SessionStatus::Creating => "starting".to_string(),
        SessionStatus::Running => "working".to_string(),
        SessionStatus::Paused => "paused".to_string(),
        SessionStatus::NeedsInput => "needs input".to_string(),
        SessionStatus::Exited => "complete".to_string(),
        SessionStatus::Failed => session.error.clone().unwrap_or_else(|| "blocked".to_string()),
        SessionStatus::UnknownRecovered => "daemon lost the live process".to_string(),
    }
}

fn subtle_style() -> Style {
    Style::default().fg(Color::DarkGray)
}

fn matches_query<T: AsRef<str>>(haystack: T, query: &str) -> bool {
    let query = query.trim();
    if query.is_empty() {
        return true;
    }
    haystack.as_ref().to_ascii_lowercase().contains(&query.to_ascii_lowercase())
}

fn session_switcher_text(session: &SessionRecord) -> String {
    format!(
        "{} {} {} {} {} {}",
        session_elapsed_label(session),
        session.session_id,
        session.title,
        session.repo_name,
        session.branch,
        session.status_string()
    )
}

fn status_label(status: SessionStatus) -> &'static str {
    match status {
        SessionStatus::Creating => "creating",
        SessionStatus::Running => "running",
        SessionStatus::Paused => "paused",
        SessionStatus::NeedsInput => "needs_input",
        SessionStatus::Exited => "exited",
        SessionStatus::Failed => "failed",
        SessionStatus::UnknownRecovered => "unknown_recovered",
    }
}

#[cfg(test)]
mod tests {
    use super::{
        ComposerField, ComposerRow, RuntimeApp, TerminalSurface, matches_query, session_icon,
        session_icon_color, session_rank, session_status_text, session_switcher_text,
    };
    use agentd_shared::paths::AppPaths;
    use agentd_shared::session::{
        AttentionLevel, GitSyncStatus, IntegrationState, SessionMode, SessionRecord, SessionStatus,
    };
    use camino::Utf8PathBuf;
    use chrono::{Duration, Utc};
    use ratatui::style::Color;

    fn demo(status: SessionStatus, integration_state: IntegrationState) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            thread_id: Some("thread-demo".to_string()),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/repo".to_string(),
            repo_path: "/tmp/repo".to_string(),
            repo_name: "repo".to_string(),
            title: "demo".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status,
            integration_state,
            git_sync: GitSyncStatus::Unknown,
            git_status_summary: None,
            has_conflicts: false,
            pid: Some(1),
            exit_code: None,
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: None,
            created_at: now,
            updated_at: now,
            exited_at: None,
        }
    }

    fn demo_paths() -> AppPaths {
        AppPaths {
            root: Utf8PathBuf::from("/tmp/agentd"),
            socket: Utf8PathBuf::from("/tmp/agentd/agentd.sock"),
            pid_file: Utf8PathBuf::from("/tmp/agentd/agentd.pid"),
            database: Utf8PathBuf::from("/tmp/agentd/state.db"),
            config: Utf8PathBuf::from("/tmp/agentd/config.toml"),
            logs_dir: Utf8PathBuf::from("/tmp/agentd/logs"),
            worktrees_dir: Utf8PathBuf::from("/tmp/agentd/worktrees"),
        }
    }

    #[test]
    fn session_rank_prioritizes_needs_input() {
        assert!(
            session_rank(&demo(SessionStatus::NeedsInput, IntegrationState::Clean))
                < session_rank(&demo(SessionStatus::Running, IntegrationState::Clean))
        );
    }

    #[test]
    fn pending_review_uses_review_icon() {
        assert_eq!(
            session_icon(&demo(SessionStatus::Exited, IntegrationState::PendingReview)),
            "⧖"
        );
    }

    #[test]
    fn session_icons_match_requested_symbols() {
        assert_eq!(session_icon(&demo(SessionStatus::NeedsInput, IntegrationState::Clean)), "◦");
        assert_eq!(session_icon(&demo(SessionStatus::Failed, IntegrationState::Clean)), "✖");
        assert_eq!(
            session_icon(&demo(SessionStatus::UnknownRecovered, IntegrationState::Clean)),
            "⚠"
        );
        assert_eq!(session_icon(&demo(SessionStatus::Running, IntegrationState::Clean)), "●");
        assert_eq!(session_icon(&demo(SessionStatus::Paused, IntegrationState::Clean)), "⏸");
        assert_eq!(session_icon(&demo(SessionStatus::Exited, IntegrationState::Clean)), "✔");
    }

    #[test]
    fn session_icon_colors_match_requested_palette() {
        assert_eq!(
            session_icon_color(&demo(SessionStatus::NeedsInput, IntegrationState::Clean)),
            Color::Yellow
        );
        assert_eq!(
            session_icon_color(&demo(SessionStatus::Failed, IntegrationState::Clean)),
            Color::Red
        );
        assert_eq!(
            session_icon_color(&demo(SessionStatus::UnknownRecovered, IntegrationState::Clean)),
            Color::Yellow
        );
        assert_eq!(
            session_icon_color(&demo(SessionStatus::Exited, IntegrationState::PendingReview)),
            Color::Blue
        );
        assert_eq!(
            session_icon_color(&demo(SessionStatus::Running, IntegrationState::Clean)),
            Color::Green
        );
        assert_eq!(
            session_icon_color(&demo(SessionStatus::Paused, IntegrationState::Clean)),
            Color::DarkGray
        );
    }

    #[test]
    fn status_text_prefers_attention_summary() {
        let mut session = demo(SessionStatus::Running, IntegrationState::Clean);
        session.attention_summary = Some("needs eyes".to_string());
        assert_eq!(session_status_text(&session), "needs eyes");
    }

    #[test]
    fn terminal_surface_applies_basic_ansi_sequences() {
        let mut surface = TerminalSurface::new(10, 2);
        surface.process(b"\x1b[31mhello\x1b[0m");
        let rendered = surface.render_lines(false);
        assert_eq!(rendered[0].spans[0].content, "hello");
    }

    #[test]
    fn query_matching_is_case_insensitive() {
        assert!(matches_query("Auth Fix", "auth"));
    }

    #[test]
    fn composer_rows_include_create_and_matching_sessions() {
        let mut app = RuntimeApp::new(demo_paths(), None);
        let mut alpha = demo(SessionStatus::Running, IntegrationState::Clean);
        alpha.session_id = "alpha".to_string();
        alpha.title = "auth fix".to_string();
        let mut beta = demo(SessionStatus::Running, IntegrationState::Clean);
        beta.session_id = "beta".to_string();
        beta.title = "billing".to_string();
        app.sessions = vec![alpha, beta];
        app.composer.query = "auth".to_string();

        let rows = app.composer_rows();
        assert_eq!(rows[0], ComposerRow::Create);
        assert_eq!(rows[1], ComposerRow::Session("alpha".to_string()));
        assert_eq!(rows.len(), 2);
    }

    #[test]
    fn open_composer_prefers_existing_session_for_switching() {
        let mut app = RuntimeApp::new(demo_paths(), None);
        let mut session = demo(SessionStatus::Running, IntegrationState::Clean);
        session.session_id = "alpha".to_string();
        app.sessions = vec![session];
        app.composer.query = "stale".to_string();

        app.open_composer(ComposerField::Query, false);

        assert_eq!(app.composer.query, "");
        assert_eq!(app.composer.selected, 1);
    }

    #[test]
    fn open_composer_preserves_create_intent() {
        let mut app = RuntimeApp::new(demo_paths(), None);
        app.composer.query = "new task".to_string();

        app.open_composer(ComposerField::Agent, true);

        assert_eq!(app.composer.field, ComposerField::Agent);
        assert_eq!(app.composer.query, "new task");
        assert_eq!(app.composer.selected, 0);
    }

    #[test]
    fn session_switcher_text_includes_elapsed_prefix() {
        let mut session = demo(SessionStatus::Running, IntegrationState::Clean);
        session.created_at = Utc::now() - Duration::minutes(23);

        let rendered = session_switcher_text(&session);

        assert!(rendered.starts_with("23m "));
        assert!(rendered.contains("demo"));
    }

    #[test]
    fn session_switcher_text_uses_exit_time_for_finished_sessions() {
        let mut session = demo(SessionStatus::Exited, IntegrationState::Clean);
        let created_at = Utc::now() - Duration::hours(5);
        session.created_at = created_at;
        session.exited_at = Some(created_at + Duration::minutes(90));

        let rendered = session_switcher_text(&session);

        assert!(rendered.starts_with("1h "));
    }
}
