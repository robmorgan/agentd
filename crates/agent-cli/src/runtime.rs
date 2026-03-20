use std::time::Duration;

use anyhow::{Context, Result, bail};
use crossterm::{
    cursor::Show,
    event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers},
    execute,
    terminal::{Clear, ClearType},
};
use ratatui::{
    Frame, Terminal,
    backend::CrosstermBackend,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear as WidgetClear, List, ListItem, ListState, Paragraph, Wrap},
};

use agentd_shared::{
    paths::AppPaths,
    protocol::{Request, Response},
    session::{IntegrationState, SessionRecord, SessionStatus},
};

use crate::{
    CODEX_MODELS, RawModeGuard, StatusString, centered_rect, daemon_get_session,
    daemon_list_sessions, kill_session, send_request,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Command {
    SessionSwitcher,
    NewSession,
    GitStatus,
    Diff,
    StopSession,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AttachLeaderAction {
    OpenPalette,
    SessionSwitcher,
    NewSession,
    SessionDetails,
    Diff,
    StopSession,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum OverlayMode {
    Palette,
    SessionSwitcher,
    NewSession { edit_agent: bool },
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

pub enum OverlayOutcome {
    Close,
    SwitchSession(String),
}

struct InlineScreenGuard;

impl InlineScreenGuard {
    fn enter() -> Result<Self> {
        execute!(std::io::stdout(), crossterm::cursor::Hide)
            .context("failed to prepare session picker")?;
        Ok(Self)
    }
}

impl Drop for InlineScreenGuard {
    fn drop(&mut self) {
        let _ = execute!(
            std::io::stdout(),
            Show,
            Clear(ClearType::All),
            crossterm::cursor::MoveTo(0, 0)
        );
    }
}

pub async fn pick_session(paths: &AppPaths) -> Result<Option<String>> {
    let _raw_mode = RawModeGuard::new()?;
    let _screen = InlineScreenGuard::enter()?;
    let backend = CrosstermBackend::new(std::io::stdout());
    let mut terminal = Terminal::new(backend).context("failed to initialize session picker")?;
    let mut picker = SessionPicker::new(paths.clone());
    picker.refresh_sessions().await?;

    loop {
        terminal.draw(|frame| picker.render(frame))?;

        if !event::poll(Duration::from_millis(200)).context("failed to poll picker input")? {
            picker.refresh_sessions().await?;
            continue;
        }

        match event::read().context("failed to read picker input")? {
            Event::Key(key) if key.kind == KeyEventKind::Press => {
                if let Some(session_id) = picker.handle_key(key).await? {
                    terminal.clear()?;
                    return Ok(session_id);
                }
            }
            Event::Paste(data) => {
                picker.handle_paste(&data);
            }
            Event::Resize(_, _) => {}
            _ => {}
        }
    }
}

struct SessionPicker {
    paths: AppPaths,
    sessions: Vec<SessionRecord>,
    query: String,
    selected: usize,
    mode: PickerMode,
    title_input: String,
    agent_input: String,
    toast: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PickerMode {
    Browse,
    NewSession { edit_agent: bool },
}

impl SessionPicker {
    fn new(paths: AppPaths) -> Self {
        Self {
            paths,
            sessions: Vec::new(),
            query: String::new(),
            selected: 0,
            mode: PickerMode::Browse,
            title_input: String::new(),
            agent_input: "codex".to_string(),
            toast: None,
        }
    }

    async fn refresh_sessions(&mut self) -> Result<()> {
        self.sessions = daemon_list_sessions(&self.paths).await?;
        if self.selected >= self.filtered_sessions().len() {
            self.selected = self.filtered_sessions().len().saturating_sub(1);
        }
        Ok(())
    }

    async fn handle_key(&mut self, key: KeyEvent) -> Result<Option<Option<String>>> {
        match self.mode {
            PickerMode::Browse => self.handle_browse_key(key).await,
            PickerMode::NewSession { edit_agent } => {
                self.handle_new_session_key(key, edit_agent).await
            }
        }
    }

    fn handle_paste(&mut self, data: &str) {
        if let PickerMode::NewSession { edit_agent } = self.mode {
            if edit_agent {
                self.agent_input.push_str(data);
            } else {
                self.title_input.push_str(data);
            }
        } else {
            self.query.push_str(data);
        }
    }

    async fn handle_browse_key(&mut self, key: KeyEvent) -> Result<Option<Option<String>>> {
        let matches = self.filtered_sessions();
        match key.code {
            KeyCode::Esc | KeyCode::Char('q') => return Ok(Some(None)),
            KeyCode::Up => self.selected = self.selected.saturating_sub(1),
            KeyCode::Down => {
                if self.selected + 1 < matches.len() {
                    self.selected += 1;
                }
            }
            KeyCode::Backspace => {
                self.query.pop();
                self.selected = 0;
            }
            KeyCode::Enter => {
                if let Some(session) = matches.get(self.selected) {
                    return Ok(Some(Some(session.session_id.clone())));
                }
            }
            KeyCode::Char('n') if key.modifiers.is_empty() => {
                self.mode = PickerMode::NewSession { edit_agent: false };
                self.title_input.clear();
                self.agent_input = "codex".to_string();
            }
            KeyCode::Char('N') => {
                self.mode = PickerMode::NewSession { edit_agent: true };
                self.title_input.clear();
                self.agent_input = "codex".to_string();
            }
            KeyCode::Char('r') if key.modifiers.is_empty() => {
                self.refresh_sessions().await?;
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.query.push(ch);
                self.selected = 0;
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_new_session_key(
        &mut self,
        key: KeyEvent,
        mut edit_agent: bool,
    ) -> Result<Option<Option<String>>> {
        match key.code {
            KeyCode::Esc => self.mode = PickerMode::Browse,
            KeyCode::Tab => {
                edit_agent = !edit_agent;
                self.mode = PickerMode::NewSession { edit_agent };
            }
            KeyCode::Backspace => {
                if edit_agent {
                    self.agent_input.pop();
                } else {
                    self.title_input.pop();
                }
            }
            KeyCode::Enter => {
                let title = self.title_input.trim();
                let agent = self.agent_input.trim();
                if agent.is_empty() {
                    self.toast = Some("agent cannot be empty".to_string());
                    return Ok(None);
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
                        return Ok(Some(Some(session.session_id)));
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
                    self.title_input.push(ch);
                }
            }
            _ => {}
        }
        Ok(None)
    }

    fn render(&self, frame: &mut Frame) {
        let area = frame.area();
        let popup = centered_rect(80, 80, area);
        frame.render_widget(WidgetClear, popup);
        let block = Block::default().borders(Borders::ALL).title("Sessions");
        let inner = block.inner(popup);
        frame.render_widget(block, popup);

        match self.mode {
            PickerMode::Browse => self.render_browser(frame, inner),
            PickerMode::NewSession { edit_agent } => {
                self.render_new_session(frame, inner, edit_agent)
            }
        }
    }

    fn render_browser(&self, frame: &mut Frame, area: ratatui::layout::Rect) {
        let filtered = self.filtered_sessions();
        let mut state = ListState::default();
        state.select(Some(self.selected.min(filtered.len().saturating_sub(1))));
        let items = filtered
            .iter()
            .map(|session| {
                ListItem::new(Line::from(vec![
                    Span::styled(
                        session_icon(session),
                        Style::default().fg(session_icon_color(session)),
                    ),
                    Span::raw("  "),
                    Span::styled(
                        session.title.as_str(),
                        Style::default().add_modifier(Modifier::BOLD),
                    ),
                    Span::raw("  "),
                    Span::styled(session.repo_name.as_str(), subtle_style()),
                    Span::raw("  "),
                    Span::styled(session.branch.as_str(), subtle_style()),
                ]))
            })
            .collect::<Vec<_>>();

        let chunks = ratatui::layout::Layout::default()
            .direction(ratatui::layout::Direction::Vertical)
            .constraints([
                ratatui::layout::Constraint::Length(2),
                ratatui::layout::Constraint::Min(5),
                ratatui::layout::Constraint::Length(2),
                ratatui::layout::Constraint::Length(1),
            ])
            .split(area);

        let query = Paragraph::new(Line::from(vec![
            Span::styled("> ", Style::default().fg(Color::Cyan)),
            Span::raw(self.query.as_str()),
        ]))
        .block(Block::default().title("Filter"));
        frame.render_widget(query, chunks[0]);
        frame.render_stateful_widget(
            List::new(items)
                .highlight_style(
                    Style::default()
                        .fg(Color::Cyan)
                        .add_modifier(Modifier::BOLD),
                )
                .highlight_symbol("› "),
            chunks[1],
            &mut state,
        );
        frame.render_widget(
            Paragraph::new("Enter attach  n new  N custom agent  r refresh  Esc quit")
                .wrap(Wrap { trim: false }),
            chunks[2],
        );
        if let Some(toast) = &self.toast {
            frame.render_widget(Paragraph::new(toast.as_str()), chunks[3]);
        }
    }

    fn render_new_session(&self, frame: &mut Frame, area: ratatui::layout::Rect, edit_agent: bool) {
        let chunks = ratatui::layout::Layout::default()
            .direction(ratatui::layout::Direction::Vertical)
            .constraints([
                ratatui::layout::Constraint::Length(3),
                ratatui::layout::Constraint::Length(3),
                ratatui::layout::Constraint::Length(2),
                ratatui::layout::Constraint::Length(1),
            ])
            .split(area);
        let title_style = if edit_agent {
            Style::default()
        } else {
            Style::default().fg(Color::Cyan)
        };
        let agent_style = if edit_agent {
            Style::default().fg(Color::Cyan)
        } else {
            Style::default()
        };
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("title  ", subtle_style()),
                Span::styled(self.title_input.as_str(), title_style),
            ]))
            .block(Block::default().borders(Borders::ALL)),
            chunks[0],
        );
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("agent  ", subtle_style()),
                Span::styled(self.agent_input.as_str(), agent_style),
            ]))
            .block(Block::default().borders(Borders::ALL)),
            chunks[1],
        );
        frame.render_widget(
            Paragraph::new("Tab switches field. Enter creates and attaches."),
            chunks[2],
        );
        if let Some(toast) = &self.toast {
            frame.render_widget(Paragraph::new(toast.as_str()), chunks[3]);
        }
    }

    fn filtered_sessions(&self) -> Vec<&SessionRecord> {
        let mut sessions = self
            .sessions
            .iter()
            .filter(|session| matches_query(session_search_text(session), &self.query))
            .collect::<Vec<_>>();
        sessions.sort_by(|left, right| {
            session_rank(left)
                .cmp(&session_rank(right))
                .then_with(|| right.updated_at.cmp(&left.updated_at))
        });
        sessions
    }
}

pub struct AttachOverlay {
    paths: AppPaths,
    session_id: String,
    sessions: Vec<SessionRecord>,
    mode: OverlayMode,
    palette_query: String,
    palette_selected: usize,
    switcher_query: String,
    switcher_selected: usize,
    title_input: String,
    agent_input: String,
    detail_text: String,
    detail_scroll: u16,
    toast: Option<String>,
}

impl AttachOverlay {
    pub fn new(paths: AppPaths, session_id: String) -> Self {
        Self {
            paths,
            session_id,
            sessions: Vec::new(),
            mode: OverlayMode::Palette,
            palette_query: String::new(),
            palette_selected: 0,
            switcher_query: String::new(),
            switcher_selected: 0,
            title_input: String::new(),
            agent_input: "codex".to_string(),
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        }
    }

    pub async fn open(&mut self) -> Result<()> {
        self.mode = OverlayMode::Palette;
        self.palette_query.clear();
        self.palette_selected = 0;
        self.switcher_query.clear();
        self.switcher_selected = 0;
        self.toast = None;
        self.refresh_sessions().await
    }

    pub async fn open_leader_action(
        &mut self,
        action: AttachLeaderAction,
    ) -> Result<Option<OverlayOutcome>> {
        self.open().await?;
        match action {
            AttachLeaderAction::OpenPalette => Ok(None),
            AttachLeaderAction::SessionSwitcher => self.run_command(Command::SessionSwitcher).await,
            AttachLeaderAction::NewSession => self.run_command(Command::NewSession).await,
            AttachLeaderAction::SessionDetails => self.run_command(Command::GitStatus).await,
            AttachLeaderAction::Diff => self.run_command(Command::Diff).await,
            AttachLeaderAction::StopSession => self.run_command(Command::StopSession).await,
        }
    }

    pub async fn handle_event(&mut self, event: Event) -> Result<Option<OverlayOutcome>> {
        match event {
            Event::Key(key) if key.kind == KeyEventKind::Press => self.handle_key(key).await,
            Event::Paste(data) => {
                self.handle_paste(&data);
                Ok(None)
            }
            Event::Resize(_, _) => Ok(None),
            _ => Ok(None),
        }
    }

    pub fn draw(&self) -> Result<()> {
        let backend = CrosstermBackend::new(std::io::stdout());
        let mut terminal = Terminal::new(backend).context("failed to initialize overlay")?;
        terminal.draw(|frame| self.render(frame))?;
        Ok(())
    }

    async fn refresh_sessions(&mut self) -> Result<()> {
        self.sessions = daemon_list_sessions(&self.paths).await?;
        if self.switcher_selected >= self.filtered_sessions().len() {
            self.switcher_selected = self.filtered_sessions().len().saturating_sub(1);
        }
        Ok(())
    }

    async fn handle_key(&mut self, key: KeyEvent) -> Result<Option<OverlayOutcome>> {
        if matches!(key.code, KeyCode::Esc)
            && !matches!(
                self.mode,
                OverlayMode::GitStatus | OverlayMode::Diff | OverlayMode::Palette
            )
        {
            self.mode = OverlayMode::Palette;
            return Ok(None);
        }

        match self.mode {
            OverlayMode::Palette => self.handle_palette_key(key).await,
            OverlayMode::SessionSwitcher => self.handle_switcher_key(key).await,
            OverlayMode::NewSession { edit_agent } => {
                self.handle_new_session_key(key, edit_agent).await
            }
            OverlayMode::GitStatus | OverlayMode::Diff => self.handle_detail_key(key).await,
            OverlayMode::StopConfirm => self.handle_stop_confirm_key(key).await,
        }
    }

    fn handle_paste(&mut self, data: &str) {
        if let OverlayMode::NewSession { edit_agent } = self.mode {
            if edit_agent {
                self.agent_input.push_str(data);
            } else {
                self.title_input.push_str(data);
            }
        } else if matches!(self.mode, OverlayMode::Palette) {
            self.palette_query.push_str(data);
        } else if matches!(self.mode, OverlayMode::SessionSwitcher) {
            self.switcher_query.push_str(data);
        }
    }

    async fn handle_palette_key(&mut self, key: KeyEvent) -> Result<Option<OverlayOutcome>> {
        let items = filtered_palette_items(&self.palette_query);
        match key.code {
            KeyCode::Esc => return Ok(Some(OverlayOutcome::Close)),
            KeyCode::Char('b') if key.modifiers.contains(KeyModifiers::CONTROL) => {
                return Ok(Some(OverlayOutcome::Close));
            }
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
                    return self.run_command(item.command).await;
                }
            }
            KeyCode::Char('s') if key.modifiers.is_empty() => {
                return self.run_command(Command::SessionSwitcher).await;
            }
            KeyCode::Char('t') if key.modifiers.is_empty() => {
                return self.run_command(Command::NewSession).await;
            }
            KeyCode::Char('g') if key.modifiers.is_empty() => {
                return self.run_command(Command::GitStatus).await;
            }
            KeyCode::Char('d') if key.modifiers.is_empty() => {
                return self.run_command(Command::Diff).await;
            }
            KeyCode::Char('x') if key.modifiers.is_empty() => {
                return self.run_command(Command::StopSession).await;
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.palette_query.push(ch);
                self.palette_selected = 0;
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_switcher_key(&mut self, key: KeyEvent) -> Result<Option<OverlayOutcome>> {
        let matches = self.filtered_sessions();
        match key.code {
            KeyCode::Esc => {
                self.mode = OverlayMode::Palette;
            }
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
                    return Ok(Some(OverlayOutcome::SwitchSession(
                        session.session_id.clone(),
                    )));
                }
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.switcher_query.push(ch);
                self.switcher_selected = 0;
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_new_session_key(
        &mut self,
        key: KeyEvent,
        mut edit_agent: bool,
    ) -> Result<Option<OverlayOutcome>> {
        match key.code {
            KeyCode::Esc => self.mode = OverlayMode::Palette,
            KeyCode::Tab => {
                edit_agent = !edit_agent;
                self.mode = OverlayMode::NewSession { edit_agent };
            }
            KeyCode::Backspace => {
                if edit_agent {
                    self.agent_input.pop();
                } else {
                    self.title_input.pop();
                }
            }
            KeyCode::Enter => {
                let title = self.title_input.trim();
                let agent = self.agent_input.trim();
                if agent.is_empty() {
                    self.toast = Some("agent cannot be empty".to_string());
                    return Ok(None);
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
                        return Ok(Some(OverlayOutcome::SwitchSession(session.session_id)));
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
                    self.title_input.push(ch);
                }
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_detail_key(&mut self, key: KeyEvent) -> Result<Option<OverlayOutcome>> {
        match key.code {
            KeyCode::Esc => self.mode = OverlayMode::Palette,
            KeyCode::Up => self.detail_scroll = self.detail_scroll.saturating_sub(1),
            KeyCode::Down => self.detail_scroll = self.detail_scroll.saturating_add(1),
            KeyCode::PageUp => self.detail_scroll = self.detail_scroll.saturating_sub(10),
            KeyCode::PageDown => self.detail_scroll = self.detail_scroll.saturating_add(10),
            _ => {}
        }
        Ok(None)
    }

    async fn handle_stop_confirm_key(&mut self, key: KeyEvent) -> Result<Option<OverlayOutcome>> {
        match key.code {
            KeyCode::Esc => self.mode = OverlayMode::Palette,
            KeyCode::Enter => {
                kill_session(&self.paths, &self.session_id).await?;
                return Ok(Some(OverlayOutcome::Close));
            }
            _ => {}
        }
        Ok(None)
    }

    async fn run_command(&mut self, command: Command) -> Result<Option<OverlayOutcome>> {
        match command {
            Command::SessionSwitcher => {
                self.refresh_sessions().await?;
                self.mode = OverlayMode::SessionSwitcher;
                self.switcher_query.clear();
                self.switcher_selected = 0;
            }
            Command::NewSession => {
                self.mode = OverlayMode::NewSession { edit_agent: false };
                self.title_input.clear();
                self.agent_input = "codex".to_string();
            }
            Command::GitStatus => {
                let session = daemon_get_session(&self.paths, &self.session_id).await?;
                let sync = session.git_sync.as_str();
                let summary = session.git_status_summary.clone().unwrap_or_else(|| {
                    "TODO: live git status sync not implemented yet".to_string()
                });
                self.detail_text = format!(
                    "repo      {}\nrepo_path  {}\nworktree   {}\nbranch     {}\nbase       {}\ngit_sync   {}\nconflicts  {}\nstatus     {}\n\n{}",
                    session.repo_name,
                    session.repo_path,
                    session.worktree,
                    session.branch,
                    session.base_branch,
                    sync,
                    if session.has_conflicts { "yes" } else { "no" },
                    session_status_text(&session),
                    summary,
                );
                self.detail_scroll = 0;
                self.mode = OverlayMode::GitStatus;
            }
            Command::Diff => {
                let response = send_request(
                    &self.paths,
                    &Request::DiffSession {
                        session_id: self.session_id.clone(),
                    },
                )
                .await?;
                match response {
                    Response::Diff { diff } => {
                        self.detail_text = diff.diff;
                        self.detail_scroll = 0;
                        self.mode = OverlayMode::Diff;
                    }
                    Response::Error { message } => self.toast = Some(message),
                    other => bail!("unexpected response: {:?}", other),
                }
            }
            Command::StopSession => {
                self.mode = OverlayMode::StopConfirm;
            }
        }
        Ok(None)
    }

    fn render(&self, frame: &mut Frame) {
        let area = centered_rect(80, 80, frame.area());
        frame.render_widget(WidgetClear, area);
        let title = match self.mode {
            OverlayMode::Palette => "Overlay",
            OverlayMode::SessionSwitcher => "Switch Session",
            OverlayMode::NewSession { .. } => "New Session",
            OverlayMode::GitStatus => "Session Details",
            OverlayMode::Diff => "Diff",
            OverlayMode::StopConfirm => "Stop Session",
        };
        let block = Block::default().borders(Borders::ALL).title(title);
        let inner = block.inner(area);
        frame.render_widget(block, area);

        match self.mode {
            OverlayMode::Palette => self.render_palette(frame, inner),
            OverlayMode::SessionSwitcher => self.render_switcher(frame, inner),
            OverlayMode::NewSession { edit_agent } => {
                self.render_new_session(frame, inner, edit_agent)
            }
            OverlayMode::GitStatus | OverlayMode::Diff => self.render_detail(frame, inner),
            OverlayMode::StopConfirm => self.render_stop_confirm(frame, inner),
        }
    }

    fn render_palette(&self, frame: &mut Frame, area: ratatui::layout::Rect) {
        let items = filtered_palette_items(&self.palette_query);
        let lines = items
            .iter()
            .enumerate()
            .map(|(index, item)| {
                let style = if index == self.palette_selected {
                    Style::default()
                        .fg(Color::Cyan)
                        .add_modifier(Modifier::BOLD)
                } else {
                    Style::default()
                };
                Line::from(vec![
                    Span::styled(format!("{:>2}", item.key_hint), subtle_style()),
                    Span::raw("  "),
                    Span::styled(item.title, style),
                ])
            })
            .collect::<Vec<_>>();
        let chunks = ratatui::layout::Layout::default()
            .direction(ratatui::layout::Direction::Vertical)
            .constraints([
                ratatui::layout::Constraint::Length(2),
                ratatui::layout::Constraint::Min(5),
                ratatui::layout::Constraint::Length(1),
            ])
            .split(area);
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("> ", Style::default().fg(Color::Cyan)),
                Span::raw(self.palette_query.as_str()),
            ])),
            chunks[0],
        );
        frame.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), chunks[1]);
        if let Some(toast) = &self.toast {
            frame.render_widget(Paragraph::new(toast.as_str()), chunks[2]);
        }
    }

    fn render_switcher(&self, frame: &mut Frame, area: ratatui::layout::Rect) {
        let sessions = self.filtered_sessions();
        let lines = sessions
            .iter()
            .enumerate()
            .map(|(index, session)| {
                let style = if index == self.switcher_selected {
                    Style::default()
                        .fg(Color::Cyan)
                        .add_modifier(Modifier::BOLD)
                } else {
                    Style::default()
                };
                Line::from(vec![
                    Span::styled(
                        session_icon(session),
                        Style::default().fg(session_icon_color(session)),
                    ),
                    Span::raw("  "),
                    Span::styled(session.title.as_str(), style),
                    Span::raw("  "),
                    Span::styled(session.repo_name.as_str(), subtle_style()),
                    Span::raw("  "),
                    Span::styled(session.branch.as_str(), subtle_style()),
                ])
            })
            .collect::<Vec<_>>();
        let chunks = ratatui::layout::Layout::default()
            .direction(ratatui::layout::Direction::Vertical)
            .constraints([
                ratatui::layout::Constraint::Length(2),
                ratatui::layout::Constraint::Min(5),
            ])
            .split(area);
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("> ", Style::default().fg(Color::Cyan)),
                Span::raw(self.switcher_query.as_str()),
            ])),
            chunks[0],
        );
        frame.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), chunks[1]);
    }

    fn render_new_session(&self, frame: &mut Frame, area: ratatui::layout::Rect, edit_agent: bool) {
        let chunks = ratatui::layout::Layout::default()
            .direction(ratatui::layout::Direction::Vertical)
            .constraints([
                ratatui::layout::Constraint::Length(3),
                ratatui::layout::Constraint::Length(3),
                ratatui::layout::Constraint::Length(2),
                ratatui::layout::Constraint::Length(1),
            ])
            .split(area);
        let title_style = if edit_agent {
            Style::default()
        } else {
            Style::default().fg(Color::Cyan)
        };
        let agent_style = if edit_agent {
            Style::default().fg(Color::Cyan)
        } else {
            Style::default()
        };
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("title  ", subtle_style()),
                Span::styled(self.title_input.as_str(), title_style),
            ]))
            .block(Block::default().borders(Borders::ALL)),
            chunks[0],
        );
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("agent  ", subtle_style()),
                Span::styled(self.agent_input.as_str(), agent_style),
            ]))
            .block(Block::default().borders(Borders::ALL)),
            chunks[1],
        );
        frame.render_widget(
            Paragraph::new("Tab switches field. Enter creates a new worker."),
            chunks[2],
        );
        if let Some(toast) = &self.toast {
            frame.render_widget(Paragraph::new(toast.as_str()), chunks[3]);
        }
    }

    fn render_detail(&self, frame: &mut Frame, area: ratatui::layout::Rect) {
        frame.render_widget(
            Paragraph::new(self.detail_text.as_str())
                .scroll((self.detail_scroll, 0))
                .wrap(Wrap { trim: false }),
            area,
        );
    }

    fn render_stop_confirm(&self, frame: &mut Frame, area: ratatui::layout::Rect) {
        frame.render_widget(
            Paragraph::new("Press Enter to stop the current session, or Esc to cancel.")
                .wrap(Wrap { trim: false }),
            area,
        );
    }

    fn filtered_sessions(&self) -> Vec<&SessionRecord> {
        let mut sessions = self
            .sessions
            .iter()
            .filter(|session| matches_query(session_search_text(session), &self.switcher_query))
            .collect::<Vec<_>>();
        sessions.sort_by(|left, right| {
            session_rank(left)
                .cmp(&session_rank(right))
                .then_with(|| right.updated_at.cmp(&left.updated_at))
        });
        sessions
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
        PaletteItem {
            key_hint: "s",
            title: "Switch Session",
            command: Command::SessionSwitcher,
        },
        PaletteItem {
            key_hint: "t",
            title: "New Session",
            command: Command::NewSession,
        },
        PaletteItem {
            key_hint: "g",
            title: "Session Details",
            command: Command::GitStatus,
        },
        PaletteItem {
            key_hint: "d",
            title: "Diff",
            command: Command::Diff,
        },
        PaletteItem {
            key_hint: "x",
            title: "Stop Session",
            command: Command::StopSession,
        },
    ]
}

fn session_search_text(session: &SessionRecord) -> String {
    format!(
        "{} {} {} {} {} {}",
        session.title,
        session.repo_name,
        session.branch,
        session.status_string(),
        session.integration_string(),
        session.attention_string()
    )
}

fn session_rank(session: &SessionRecord) -> u8 {
    if session.status == SessionStatus::NeedsInput {
        0
    } else if matches!(
        session.status,
        SessionStatus::Failed | SessionStatus::UnknownRecovered
    ) || session.has_conflicts
    {
        1
    } else if session.integration_state == IntegrationState::PendingReview {
        2
    } else if matches!(
        session.status,
        SessionStatus::Running | SessionStatus::Creating
    ) {
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
            SessionStatus::Exited => "✓",
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
    match session.status {
        SessionStatus::Creating => "starting".to_string(),
        SessionStatus::Running => "running".to_string(),
        SessionStatus::Paused => "paused".to_string(),
        SessionStatus::NeedsInput => "needs input".to_string(),
        SessionStatus::Exited => "complete".to_string(),
        SessionStatus::Failed => session
            .error
            .clone()
            .unwrap_or_else(|| "blocked".to_string()),
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
