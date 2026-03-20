use std::{
    collections::VecDeque,
    time::Duration,
};

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
    session::{IntegrationState, SessionMode, SessionRecord, SessionStatus},
};

use crate::{
    CODEX_MODELS, RawModeGuard, StatusString, TerminalScreenGuard, centered_rect,
    daemon_get_session, daemon_list_sessions, encode_attach_key, kill_session, send_request,
};

const LEADER_KEY: (KeyCode, KeyModifiers) = (KeyCode::Char('b'), KeyModifiers::CONTROL);
const SIDEBAR_WIDTH: u16 = 34;
const CARD_HEIGHT: u16 = 6;
const MAX_PTY_LINES: usize = 1200;
const MAX_PTY_CHARS: usize = 200_000;

pub async fn run_runtime_ui(paths: &AppPaths, initial_session_id: Option<&str>) -> Result<()> {
    let _raw_mode = RawModeGuard::new()?;
    let _screen = TerminalScreenGuard::enter()?;
    let backend = CrosstermBackend::new(std::io::stdout());
    let mut terminal = Terminal::new(backend).context("failed to initialize terminal")?;
    let mut app = RuntimeApp::new(paths.clone(), initial_session_id);

    loop {
        app.refresh_sessions().await?;
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
    SessionSwitcher,
    NewTask { edit_agent: bool },
    WorktreeActions,
    GitStatus,
    Diff,
    StopConfirm,
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
    NewTask,
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
    switcher_query: String,
    switcher_selected: usize,
    task_input: String,
    agent_input: String,
    diff_text: String,
    diff_scroll: u16,
    detail_text: String,
    detail_scroll: u16,
    toast: Option<String>,
    pty: Option<FocusedPty>,
    last_pane_size: Option<(u16, u16)>,
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
            switcher_query: String::new(),
            switcher_selected: 0,
            task_input: String::new(),
            agent_input: "codex".to_string(),
            diff_text: String::new(),
            diff_scroll: 0,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
            pty: None,
            last_pane_size: None,
        }
    }

    async fn refresh_sessions(&mut self) -> Result<()> {
        self.sessions = daemon_list_sessions(&self.paths).await?;
        let existing = self
            .sessions
            .iter()
            .any(|session| matches!(&self.focus, FocusTarget::Worker(id) if id == &session.session_id));
        if matches!(self.focus, FocusTarget::Worker(_)) && !existing {
            self.focus = FocusTarget::Coordinator;
        }
        Ok(())
    }

    async fn sync_focus_runtime(&mut self) -> Result<()> {
        let live_session_id = self.focused_worker_session_id().map(str::to_string);
        match live_session_id {
            Some(session_id) => {
                let live_session = self.sessions.iter().find(|session| session.session_id == session_id);
                let live_status = live_session.map(|session| session.status);
                let needs_attach = self
                    .pty
                    .as_ref()
                    .map(|pty| pty.session_id != session_id)
                    .unwrap_or(true);

                if needs_attach {
                    self.stop_pty();
                    if matches!(live_status, Some(SessionStatus::Running | SessionStatus::NeedsInput)) {
                        self.pty = Some(FocusedPty::spawn(self.paths.clone(), session_id.clone()));
                    }
                }

                if !matches!(live_status, Some(SessionStatus::Running | SessionStatus::NeedsInput)) {
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
        self.sessions
            .iter()
            .find(|session| session.session_id == session_id)
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
        let targets = if attention_only {
            self.attention_targets()
        } else {
            self.ordered_targets()
        };
        if targets.is_empty() {
            return;
        }
        let current_index = targets
            .iter()
            .position(|target| *target == self.focus)
            .unwrap_or(0);
        let len = targets.len() as isize;
        let next = (current_index as isize + direction).rem_euclid(len) as usize;
        self.focus = targets[next].clone();
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
                    PtyEvent::Snapshot(text) => pty.replace(text),
                    PtyEvent::Output(text) => pty.push(text),
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
            KeyCode::Char('t') => self.run_command(Command::NewTask).await?,
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
            Modal::SessionSwitcher => {
                self.handle_switcher_key(key);
                Ok(true)
            }
            Modal::NewTask { edit_agent } => {
                self.handle_task_modal_key(key, edit_agent).await?;
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
            Command::SessionSwitcher => {
                self.modal = Modal::SessionSwitcher;
                self.switcher_query.clear();
                self.switcher_selected = 0;
            }
            Command::NewTask => {
                self.modal = Modal::NewTask { edit_agent: false };
                self.task_input.clear();
                self.agent_input = "codex".to_string();
            }
            Command::NewAgent => {
                self.modal = Modal::NewTask { edit_agent: true };
                self.task_input.clear();
                self.agent_input = "codex".to_string();
            }
            Command::WorktreeActions => {
                self.modal = Modal::WorktreeActions;
                self.detail_text = "fetch\nrebase main\nmerge main\ndetect conflicts\n\nTODO: wire these to thin daemon git helpers without turning agentd into a large git automation layer.".to_string();
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
        let response = send_request(
            &self.paths,
            &Request::DiffSession {
                session_id: session_id.to_string(),
            },
        )
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

    fn handle_switcher_key(&mut self, key: KeyEvent) {
        let matches = self.filtered_sessions();
        match key.code {
            KeyCode::Esc => self.close_modal(),
            KeyCode::Up => self.switcher_selected = self.switcher_selected.saturating_sub(1),
            KeyCode::Down => {
                if self.switcher_selected + 1 < matches.len() {
                    self.switcher_selected += 1;
                }
            }
            KeyCode::Backspace => {
                self.switcher_query.pop();
                self.switcher_selected = 0;
            }
            KeyCode::Enter => {
                if let Some(session) = matches.get(self.switcher_selected) {
                    self.focus = FocusTarget::Worker(session.session_id.clone());
                }
                self.close_modal();
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.switcher_query.push(ch);
                self.switcher_selected = 0;
            }
            _ => {}
        }
    }

    async fn handle_task_modal_key(&mut self, key: KeyEvent, mut edit_agent: bool) -> Result<()> {
        match key.code {
            KeyCode::Esc => self.close_modal(),
            KeyCode::Tab => {
                edit_agent = !edit_agent;
                self.modal = Modal::NewTask { edit_agent };
            }
            KeyCode::Backspace => {
                if edit_agent {
                    self.agent_input.pop();
                } else {
                    self.task_input.pop();
                }
            }
            KeyCode::Enter => {
                let task = self.task_input.trim();
                let agent = self.agent_input.trim();
                if task.is_empty() {
                    self.toast = Some("task cannot be empty".to_string());
                    return Ok(());
                }
                if agent.is_empty() {
                    self.toast = Some("agent cannot be empty".to_string());
                    return Ok(());
                }
                let workspace = std::env::current_dir().context("failed to determine current directory")?;
                let response = send_request(
                    &self.paths,
                    &Request::CreateSession {
                        workspace: workspace.to_string_lossy().to_string(),
                        task: task.to_string(),
                        agent: agent.to_string(),
                        model: if agent == "codex" {
                            Some(CODEX_MODELS[0].to_string())
                        } else {
                            None
                        },
                        mode: SessionMode::Execute,
                    },
                )
                .await?;
                match response {
                    Response::CreateSession { session } => {
                        let created = daemon_get_session(&self.paths, &session.session_id).await?;
                        self.focus = FocusTarget::Worker(created.session_id);
                        self.toast = Some(format!("created {}", created.task));
                        self.close_modal();
                    }
                    Response::Error { message } => self.toast = Some(message),
                    other => bail!("unexpected response: {:?}", other),
                }
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                if edit_agent {
                    self.agent_input.push(ch);
                } else {
                    self.task_input.push(ch);
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
        self.switcher_query.clear();
        self.switcher_selected = 0;
        self.detail_scroll = 0;
    }

    fn filtered_sessions(&self) -> Vec<&SessionRecord> {
        let mut sessions = self
            .ordered_sessions()
            .into_iter()
            .filter(|session| matches_query(session_switcher_text(session), &self.switcher_query))
            .collect::<Vec<_>>();
        sessions.sort_by(|left, right| {
            session_rank(left)
                .cmp(&session_rank(right))
                .then_with(|| right.updated_at.cmp(&left.updated_at))
        });
        sessions
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
            let is_focused = matches!(&self.focus, FocusTarget::Worker(id) if id == &session.session_id);
            let card = SidebarCard::from_session(session, is_focused);
            self.render_session_card(
                frame,
                Rect::new(area.x + 1, y, area.width.saturating_sub(2), CARD_HEIGHT.saturating_sub(1)),
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
                Span::styled(card.icon, Style::default().fg(card.icon_color).add_modifier(Modifier::BOLD)),
                Span::raw("  "),
                Span::styled(card.title, Style::default().add_modifier(Modifier::BOLD)),
            ]),
            Line::from(vec![
                Span::styled("repo    ", subtle_style()),
                Span::raw(card.repo_name),
            ]),
            Line::from(vec![
                Span::styled("branch  ", subtle_style()),
                Span::raw(card.branch),
            ]),
            Line::from(Span::styled(card.status_text, Style::default().fg(card.icon_color))),
        ];
        frame.render_widget(
            Paragraph::new(lines).wrap(Wrap { trim: false }),
            inner,
        );
    }

    fn render_main(&mut self, frame: &mut Frame, area: Rect) {
        match self.focused_session().cloned() {
            Some(session) => self.render_worker(frame, area, &session),
            None => self.render_coordinator(frame, area),
        }
    }

    fn render_coordinator(&self, frame: &mut Frame, area: Rect) {
        let block = Block::default()
            .title("Coordinator")
            .borders(Borders::ALL);
        let inner = block.inner(area);
        frame.render_widget(block, area);

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
                    Span::styled(session_icon(session), Style::default().fg(session_icon_color(session))),
                    Span::raw("  "),
                    Span::styled(session.task.as_str(), Style::default().add_modifier(Modifier::BOLD)),
                    Span::raw("  "),
                    Span::styled(session.repo_name.as_str(), subtle_style()),
                    Span::raw("  "),
                    Span::styled(session.branch.as_str(), subtle_style()),
                ]));
                lines.push(Line::from(Span::styled(
                    session_status_text(session),
                    subtle_style(),
                )));
            }
        }

        lines.push(Line::from(""));
        lines.push(Line::from(Span::styled(
            "TODO: richer coordinator summaries, cross-session review queues, and grouped worktree actions.",
            subtle_style(),
        )));

        frame.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
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
                Span::styled(session_icon(session), Style::default().fg(session_icon_color(session)).add_modifier(Modifier::BOLD)),
                Span::raw("  "),
                Span::styled(session.task.as_str(), Style::default().add_modifier(Modifier::BOLD)),
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

        let mut body = if let Some(pty) = &self.pty {
            if pty.session_id == session.session_id {
                pty.rendered_text()
            } else {
                String::new()
            }
        } else {
            String::new()
        };

        if body.trim().is_empty() {
            let path = super::resolve_focus_log_path(&self.paths, session);
            body = super::read_focus_log_contents(&path).unwrap_or_default();
        }

        if let Some(pty) = &mut self.pty
            && pty.session_id == session.session_id
        {
            let size = (pty_inner.width.max(1), pty_inner.height.max(1));
            if self.last_pane_size != Some(size) {
                self.last_pane_size = Some(size);
                let _ = pty.commands.send(PtyCommand::Resize {
                    cols: size.0,
                    rows: size.1,
                });
            }
        }

        // TODO: replace this sanitized text surface with a real inline VT cell renderer.
        frame.render_widget(
            Paragraph::new(body)
                .wrap(Wrap { trim: false })
                .block(Block::default()),
            pty_inner,
        );
    }

    fn render_overlay(&self, frame: &mut Frame, area: Rect) {
        match self.modal {
            Modal::None => {}
            Modal::Palette => self.render_palette(frame, area),
            Modal::SessionSwitcher => self.render_switcher(frame, area),
            Modal::NewTask { edit_agent } => self.render_task_modal(frame, area, edit_agent),
            Modal::WorktreeActions => self.render_detail_modal(frame, area, "Worktree Actions", &self.detail_text, self.detail_scroll),
            Modal::GitStatus => self.render_detail_modal(frame, area, "Git Status", &self.detail_text, self.detail_scroll),
            Modal::Diff => self.render_detail_modal(frame, area, "Diff", &self.diff_text, self.diff_scroll),
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

    fn render_switcher(&self, frame: &mut Frame, area: Rect) {
        let overlay = centered_rect(56, 50, area);
        let matches = self.filtered_sessions();
        let mut lines = vec![Line::from(vec![
            Span::styled("> ", Style::default().fg(Color::Cyan)),
            Span::raw(self.switcher_query.as_str()),
        ])];
        lines.push(Line::from(""));
        for (index, session) in matches.iter().take(9).enumerate() {
            let style = if index == self.switcher_selected {
                Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
            } else {
                Style::default()
            };
            lines.push(Line::from(vec![
                Span::styled(session_icon(session), Style::default().fg(session_icon_color(session))),
                Span::raw("  "),
                Span::styled(session.task.as_str(), style),
            ]));
            lines.push(Line::from(vec![
                Span::raw("   "),
                Span::styled(session.repo_name.as_str(), subtle_style()),
                Span::raw("  "),
                Span::styled(session.branch.as_str(), subtle_style()),
            ]));
        }
        frame.render_widget(WidgetClear, overlay);
        frame.render_widget(
            Paragraph::new(lines)
                .block(Block::default().borders(Borders::ALL).title("Session Switcher"))
                .wrap(Wrap { trim: false }),
            overlay,
        );
    }

    fn render_task_modal(&self, frame: &mut Frame, area: Rect, edit_agent: bool) {
        let overlay = centered_rect(56, 32, area);
        let lines = vec![
            Line::from(vec![
                Span::styled(
                    if edit_agent { "  " } else { "> " },
                    Style::default().fg(Color::Cyan),
                ),
                Span::styled("task   ", subtle_style()),
                Span::raw(self.task_input.as_str()),
            ]),
            Line::from(vec![
                Span::styled(
                    if edit_agent { "> " } else { "  " },
                    Style::default().fg(Color::Cyan),
                ),
                Span::styled("agent  ", subtle_style()),
                Span::raw(self.agent_input.as_str()),
            ]),
            Line::from(""),
            Line::from(Span::styled(
                "Enter creates, Tab switches field, Esc closes.",
                subtle_style(),
            )),
        ];
        frame.render_widget(WidgetClear, overlay);
        frame.render_widget(
            Paragraph::new(lines)
                .block(Block::default().borders(Borders::ALL).title("New Worker")),
            overlay,
        );
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
    lines: VecDeque<String>,
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
            lines: VecDeque::new(),
            connected: true,
            last_error: None,
        }
    }

    fn replace(&mut self, text: String) {
        self.lines.clear();
        self.push(text);
    }

    fn push(&mut self, text: String) {
        for line in text.lines() {
            self.lines.push_back(line.to_string());
        }
        while self.lines.len() > MAX_PTY_LINES {
            self.lines.pop_front();
        }
    }

    fn rendered_text(&self) -> String {
        let mut text = self.lines.iter().cloned().collect::<Vec<_>>().join("\n");
        if let Some(error) = &self.last_error {
            if !text.is_empty() {
                text.push_str("\n\n");
            }
            text.push_str(error);
        }
        if text.len() > MAX_PTY_CHARS {
            let start = text.len() - MAX_PTY_CHARS;
            text = text[start..].to_string();
        }
        text
    }
}

enum PtyCommand {
    Input(Vec<u8>),
    Resize { cols: u16, rows: u16 },
    Close,
}

enum PtyEvent {
    Snapshot(String),
    Output(String),
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
            },
        )
        .await?;

        let (read_half, mut write_half) = stream.into_split();
        let mut reader = BufReader::new(read_half);
        let Some(initial) = read_response(&mut reader).await? else {
            bail!("agentd closed the connection");
        };

        match initial {
            Response::Attached { snapshot } => {
                let _ = messages.send(PtyEvent::Snapshot(sanitize_terminal_bytes(&snapshot)));
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
                            let text = sanitize_terminal_bytes(&data);
                            if !text.is_empty() {
                                let _ = messages.send(PtyEvent::Output(text));
                            }
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
            title: session.task.clone(),
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
        PaletteItem { key_hint: "c", title: "Focus Coordinator", command: Command::FocusCoordinator },
        PaletteItem { key_hint: "s", title: "Session Switcher", command: Command::SessionSwitcher },
        PaletteItem { key_hint: "t", title: "New Task", command: Command::NewTask },
        PaletteItem { key_hint: "N", title: "New Agent", command: Command::NewAgent },
        PaletteItem { key_hint: "w", title: "Worktree Actions", command: Command::WorktreeActions },
        PaletteItem { key_hint: "g", title: "Git Status", command: Command::GitStatus },
        PaletteItem { key_hint: "d", title: "Diff", command: Command::Diff },
        PaletteItem { key_hint: "x", title: "Stop Session", command: Command::StopSession },
    ]
}

fn session_rank(session: &SessionRecord) -> u8 {
    if session.status == SessionStatus::NeedsInput {
        0
    } else if matches!(session.status, SessionStatus::Failed | SessionStatus::UnknownRecovered) || session.has_conflicts {
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
        "R"
    } else {
        match session.status {
            SessionStatus::NeedsInput => "?",
            SessionStatus::Failed | SessionStatus::UnknownRecovered => "!",
            SessionStatus::Running | SessionStatus::Creating | SessionStatus::Paused => "...",
            SessionStatus::Exited => "\u{2713}",
        }
    }
}

fn session_icon_color(session: &SessionRecord) -> Color {
    if session.has_conflicts {
        Color::Red
    } else if session.integration_state == IntegrationState::PendingReview {
        Color::Yellow
    } else {
        match session.status {
            SessionStatus::NeedsInput => Color::Yellow,
            SessionStatus::Failed | SessionStatus::UnknownRecovered => Color::Red,
            SessionStatus::Running | SessionStatus::Creating | SessionStatus::Paused => Color::Blue,
            SessionStatus::Exited => Color::Green,
        }
    }
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
    haystack
        .as_ref()
        .to_ascii_lowercase()
        .contains(&query.to_ascii_lowercase())
}

fn session_switcher_text(session: &SessionRecord) -> String {
    format!(
        "{} {} {} {} {}",
        session.session_id, session.task, session.repo_name, session.branch, session.status_string()
    )
}

fn sanitize_terminal_bytes(bytes: &[u8]) -> String {
    sanitize_terminal_text(&String::from_utf8_lossy(bytes))
}

fn sanitize_terminal_text(input: &str) -> String {
    let mut output = String::with_capacity(input.len());
    let mut chars = input.chars().peekable();

    while let Some(ch) = chars.next() {
        match ch {
            '\u{1b}' => {
                match chars.peek().copied() {
                    Some('[') => {
                        chars.next();
                        for next in chars.by_ref() {
                            if ('@'..='~').contains(&next) {
                                break;
                            }
                        }
                    }
                    Some(']') => {
                        chars.next();
                        let mut prev = '\0';
                        for next in chars.by_ref() {
                            if next == '\u{7}' || (prev == '\u{1b}' && next == '\\') {
                                break;
                            }
                            prev = next;
                        }
                    }
                    _ => {}
                }
            }
            '\r' => {
                if chars.peek() != Some(&'\n') {
                    output.push('\n');
                }
            }
            ch if ch == '\n' || ch == '\t' => output.push(ch),
            ch if ch.is_control() => {}
            ch => output.push(ch),
        }
    }

    if output.len() > MAX_PTY_CHARS {
        output = output[output.len() - MAX_PTY_CHARS..].to_string();
    }
    output
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
    use super::{matches_query, sanitize_terminal_text, session_icon, session_rank, session_status_text};
    use agentd_shared::session::{AttentionLevel, GitSyncStatus, IntegrationState, SessionMode, SessionRecord, SessionStatus};
    use chrono::Utc;

    fn demo(status: SessionStatus, integration_state: IntegrationState) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            thread_id: Some("thread-demo".to_string()),
            agent: "codex".to_string(),
            model: Some("gpt-5.4-codex".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/repo".to_string(),
            repo_path: "/tmp/repo".to_string(),
            repo_name: "repo".to_string(),
            task: "demo".to_string(),
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

    #[test]
    fn session_rank_prioritizes_needs_input() {
        assert!(session_rank(&demo(SessionStatus::NeedsInput, IntegrationState::Clean)) < session_rank(&demo(SessionStatus::Running, IntegrationState::Clean)));
    }

    #[test]
    fn pending_review_uses_review_icon() {
        assert_eq!(session_icon(&demo(SessionStatus::Exited, IntegrationState::PendingReview)), "R");
    }

    #[test]
    fn status_text_prefers_attention_summary() {
        let mut session = demo(SessionStatus::Running, IntegrationState::Clean);
        session.attention_summary = Some("needs eyes".to_string());
        assert_eq!(session_status_text(&session), "needs eyes");
    }

    #[test]
    fn ansi_sequences_are_stripped_from_terminal_text() {
        assert_eq!(sanitize_terminal_text("\u{1b}[31mhello\u{1b}[0m"), "hello");
    }

    #[test]
    fn query_matching_is_case_insensitive() {
        assert!(matches_query("Auth Fix", "auth"));
    }
}
