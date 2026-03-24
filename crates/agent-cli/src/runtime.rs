use std::{io::Write, time::Duration};

use anyhow::{Context, Result, bail};
use crossterm::{
    cursor::MoveTo,
    event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers},
    execute,
    style::{Color as CrosColor, Stylize},
    terminal::{self, Clear, ClearType},
};
use ratatui::{
    Frame, Terminal,
    backend::CrosstermBackend,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear as WidgetClear, Paragraph, Wrap},
};

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{Request, Response},
    session::{
        ApplyState, IntegrationPolicy, MergeStatus, SessionMode, SessionRecord, SessionStatus,
    },
};

use crate::{
    CODEX_MODELS, RawModeGuard, StatusString, centered_rect, daemon_get_session,
    daemon_list_sessions, kill_session, send_request, session_display::session_elapsed_label,
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

#[derive(Debug, PartialEq, Eq)]
pub enum OverlayOutcome {
    Close,
    ForwardInput(Vec<u8>),
    SwitchSession(String),
}

const HOST_PICKER_QUERY_BG: &str = "\x1b[48;2;62;63;71m";
const HOST_PICKER_PLACEHOLDER_FG: &str = "\x1b[38;2;151;152;153m";
const HOST_PICKER_TEXT_FG: &str = "\x1b[38;2;255;255;255m";
const HOST_PICKER_STATUS_YELLOW_FG: &str = "\x1b[38;2;250;204;21m";
const HOST_PICKER_STATUS_RED_FG: &str = "\x1b[38;2;239;68;68m";
const HOST_PICKER_STATUS_BLUE_FG: &str = "\x1b[38;2;96;165;250m";
const HOST_PICKER_STATUS_GREEN_FG: &str = "\x1b[38;2;34;197;94m";
const HOST_PICKER_DIFF_HEADER_STYLE: &str = "\x1b[38;2;153;214;255m\x1b[1m";
const HOST_PICKER_DIFF_HUNK_STYLE: &str = "\x1b[38;2;242;201;76m\x1b[1m";
const HOST_PICKER_DIFF_ADD_STYLE: &str = "\x1b[38;2;111;207;151m";
const HOST_PICKER_DIFF_REMOVE_STYLE: &str = "\x1b[38;2;255;107;107m";
const HOST_PICKER_SELECTED_STYLE: &str = "\x1b[34m";
const HOST_PICKER_LEGEND_TEXT_STYLE: &str = "\x1b[90m";
const ANSI_DIM: &str = "\x1b[2m";
const HOST_PICKER_CURSOR: &str = "█";
const HOST_PICKER_CURSOR_BLINK_MS: u128 = 500;
const ANSI_RESET: &str = "\x1b[0m";
const HOST_PICKER_ENTER_SEQUENCE: &[u8] = b"\x1b[H\x1b[2J\x1b[?25l";
const HOST_PICKER_EXIT_SEQUENCE: &[u8] = b"\x1b[H\x1b[2J\x1b[?25h";
const SESSION_LIST_DEFAULT_WIDTH: usize = 120;
const SESSION_LIST_STATUS_WIDTH: usize = 16;
const SESSION_LIST_AGE_WIDTH: usize = 6;
const SESSION_LIST_SESSION_MIN_WIDTH: usize = 8;
const SESSION_LIST_SESSION_MAX_WIDTH: usize = 14;
const SESSION_LIST_SESSION_FLOOR_WIDTH: usize = 6;
const SESSION_LIST_TITLE_MIN_WIDTH: usize = 12;
const SESSION_LIST_TITLE_MAX_WIDTH: usize = 30;
const SESSION_LIST_TITLE_FLOOR_WIDTH: usize = 8;
const SESSION_LIST_REPO_MIN_WIDTH: usize = 8;
const SESSION_LIST_REPO_MAX_WIDTH: usize = 18;
const SESSION_LIST_REPO_FLOOR_WIDTH: usize = 6;
const SESSION_LIST_BRANCH_MIN_WIDTH: usize = 12;
const SESSION_LIST_BRANCH_MAX_WIDTH: usize = 34;
const SESSION_LIST_BRANCH_FLOOR_WIDTH: usize = 8;
const SESSION_LIST_STRUCTURAL_WIDTH: usize = 12;

fn default_integration_policy(paths: &AppPaths) -> IntegrationPolicy {
    let _ = Config::load(paths);
    IntegrationPolicy::ManualReview
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct SessionListLayout {
    visible_width: usize,
    status: usize,
    age: usize,
    session: usize,
    title: usize,
    repo: usize,
    branch: usize,
}

struct PickerScreenGuard;

impl PickerScreenGuard {
    fn enter() -> Result<Self> {
        write_screen_bytes(HOST_PICKER_ENTER_SEQUENCE).context("failed to prepare session picker")?;
        Ok(Self)
    }
}

impl Drop for PickerScreenGuard {
    fn drop(&mut self) {
        let _ = write_screen_bytes(HOST_PICKER_EXIT_SEQUENCE);
    }
}

fn write_screen_bytes(bytes: &[u8]) -> Result<()> {
    let mut stdout = std::io::stdout();
    stdout.write_all(bytes).context("failed to write screen bytes")?;
    stdout.flush().context("failed to flush screen bytes")
}

fn configured_agent_names(paths: &AppPaths) -> Result<Vec<String>> {
    let mut names = Config::load(paths)?.agents.into_keys().collect::<Vec<_>>();
    if names.is_empty() {
        names.push("codex".to_string());
    }
    Ok(names)
}

fn configured_default_agent(paths: &AppPaths) -> Result<String> {
    let config = Config::load(paths)?;
    Ok(config.default_agent_name(paths)?.to_string())
}

pub async fn pick_session(paths: &AppPaths) -> Result<Option<String>> {
    let _raw_mode = RawModeGuard::new()?;
    let _screen = PickerScreenGuard::enter()?;
    let mut picker = SessionPicker::new(paths.clone());
    picker.refresh_sessions().await?;
    let mut rendered_lines = 0;
    let mut last_lines = Vec::new();
    let blink_start = std::time::Instant::now();

    loop {
        let width = terminal::size().map(|(cols, _)| cols as usize).unwrap_or(80);
        let cursor_visible =
            (blink_start.elapsed().as_millis() / HOST_PICKER_CURSOR_BLINK_MS).is_multiple_of(2);
        let lines = picker.render_lines(width, cursor_visible);
        if lines != last_lines {
            rendered_lines = draw_host_picker(&lines, rendered_lines)?;
            last_lines = lines;
        }

        if !event::poll(Duration::from_millis(200)).context("failed to poll picker input")? {
            picker.refresh_sessions().await?;
            continue;
        }

        match event::read().context("failed to read picker input")? {
            Event::Key(key) if key.kind == KeyEventKind::Press => {
                if let Some(session_id) = picker.handle_key(key).await? {
                    clear_host_picker(rendered_lines)?;
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

fn draw_host_picker(lines: &[String], previous_lines: usize) -> Result<usize> {
    clear_host_picker(previous_lines)?;
    let mut stdout = std::io::stdout();
    for (index, line) in lines.iter().enumerate() {
        if index > 0 {
            write!(stdout, "\r\n").context("failed to write session picker newline")?;
        }
        write!(stdout, "{line}").context("failed to write session picker line")?;
    }
    stdout.flush().context("failed to flush session picker")?;
    Ok(lines.len())
}

fn clear_host_picker(previous_lines: usize) -> Result<()> {
    if previous_lines == 0 {
        return Ok(());
    }
    let mut stdout = std::io::stdout();
    execute!(stdout, MoveTo(0, 0), Clear(ClearType::All))
        .context("failed to clear session picker")?;
    stdout.flush().context("failed to flush session picker clear")
}

pub(crate) fn default_session_list_width() -> usize {
    SESSION_LIST_DEFAULT_WIDTH
}

pub(crate) fn render_session_list_lines(sessions: &[SessionRecord], width: usize) -> Vec<String> {
    let layout = session_list_layout(width);
    let mut lines = vec![render_session_list_header_row(layout)];

    if sessions.is_empty() {
        lines.push(truncate_session_list_cell("  No sessions.", layout.visible_width));
        return lines;
    }

    for session in sessions {
        lines.push(render_session_list_row(session, layout));
    }

    lines
}

struct SessionPicker {
    paths: AppPaths,
    sessions: Vec<SessionRecord>,
    create_agents: Vec<String>,
    composer: PickerComposer,
    mode: PickerMode,
    detail_text: String,
    detail_scroll: usize,
    toast: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum PickerRow {
    Create,
    Session(String),
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum PickerMode {
    Browse,
    CreateAgentSelect { selected: usize },
    SessionActions { session_id: String, selected: usize },
    DiffView { session_id: String },
    DeleteConfirm { session_id: String, selected: usize },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SessionAction {
    Attach,
    Diff,
    Merge,
    Delete,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ConfirmAction {
    Yes,
    No,
}

#[derive(Clone, Debug)]
struct PickerComposer {
    query: String,
    selected: usize,
}

impl SessionPicker {
    fn new(paths: AppPaths) -> Self {
        let create_agents =
            configured_agent_names(&paths).unwrap_or_else(|_| vec!["codex".to_string()]);
        Self {
            paths,
            sessions: Vec::new(),
            create_agents,
            composer: PickerComposer { query: String::new(), selected: 0 },
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        }
    }

    async fn refresh_sessions(&mut self) -> Result<()> {
        self.sessions = daemon_list_sessions(&self.paths).await?;
        self.refresh_agent_names()?;
        self.clamp_selection();
        self.clamp_mode_selection();
        Ok(())
    }

    async fn handle_key(&mut self, key: KeyEvent) -> Result<Option<Option<String>>> {
        match self.mode.clone() {
            PickerMode::Browse => self.handle_composer_key(key).await,
            PickerMode::CreateAgentSelect { selected } => {
                self.handle_create_agent_key(key, selected).await
            }
            PickerMode::SessionActions { session_id, selected } => {
                self.handle_action_menu_key(key, session_id, selected).await
            }
            PickerMode::DiffView { session_id } => self.handle_diff_view_key(key, session_id).await,
            PickerMode::DeleteConfirm { session_id, selected } => {
                self.handle_delete_confirm_key(key, session_id, selected).await
            }
        }
    }

    fn handle_paste(&mut self, data: &str) {
        if !matches!(self.mode, PickerMode::Browse) {
            return;
        }
        self.toast = None;
        self.composer.query.push_str(data);
        self.clamp_selection();
        if self.picker_rows().len() > 1 {
            self.composer.selected = 1;
        }
    }

    async fn handle_composer_key(&mut self, key: KeyEvent) -> Result<Option<Option<String>>> {
        let row_count = self.picker_rows().len();
        match key.code {
            KeyCode::Esc | KeyCode::Char('q') => return Ok(Some(None)),
            KeyCode::Up => {
                self.composer.selected = self.composer.selected.saturating_sub(1);
            }
            KeyCode::Down => {
                if self.composer.selected + 1 < row_count {
                    self.composer.selected += 1;
                }
            }
            KeyCode::Backspace => {
                self.toast = None;
                self.composer.query.pop();
                self.clamp_selection();
            }
            KeyCode::Enter => match self.picker_rows().get(self.composer.selected).cloned() {
                Some(PickerRow::Session(session_id)) => {
                    self.open_action_menu(&session_id);
                    return Ok(None);
                }
                Some(PickerRow::Create) | None => {
                    self.open_create_agent_menu();
                }
            },
            KeyCode::Char('r') if key.modifiers.is_empty() => {
                self.refresh_sessions().await?;
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.toast = None;
                self.composer.query.push(ch);
                self.clamp_selection();
                if self.picker_rows().len() > 1 {
                    self.composer.selected = 1;
                }
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_create_agent_key(
        &mut self,
        key: KeyEvent,
        selected: usize,
    ) -> Result<Option<Option<String>>> {
        let agent_count = self.create_agents.len();
        match key.code {
            KeyCode::Esc => {
                self.mode = PickerMode::Browse;
            }
            KeyCode::Up => self.set_create_agent_selection(selected.saturating_sub(1)),
            KeyCode::Down => {
                let next = if agent_count == 0 { 0 } else { (selected + 1).min(agent_count - 1) };
                self.set_create_agent_selection(next);
            }
            KeyCode::Enter => {
                if let Some(agent) = self.create_agents.get(selected).cloned() {
                    return self.create_session_with_agent(&agent).await;
                }
            }
            KeyCode::Char(ch) if key.modifiers.is_empty() => {
                if let Some(index) = ch.to_digit(10).and_then(|value| value.checked_sub(1)) {
                    let index = index as usize;
                    if let Some(agent) = self.create_agents.get(index).cloned() {
                        return self.create_session_with_agent(&agent).await;
                    }
                }
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_action_menu_key(
        &mut self,
        key: KeyEvent,
        session_id: String,
        selected: usize,
    ) -> Result<Option<Option<String>>> {
        let action_count = self.action_items(&session_id).len();
        match key.code {
            KeyCode::Esc => {
                self.mode = PickerMode::Browse;
            }
            KeyCode::Up => self.set_action_selection(&session_id, selected.saturating_sub(1)),
            KeyCode::Down => {
                let next = if action_count == 0 { 0 } else { (selected + 1).min(action_count - 1) };
                self.set_action_selection(&session_id, next);
            }
            KeyCode::Enter => {
                if let Some(action) = self.action_items(&session_id).get(selected).copied() {
                    return self.run_session_action(session_id, action).await;
                }
            }
            KeyCode::Char(ch) if key.modifiers.is_empty() => {
                if let Some(index) = ch.to_digit(10).and_then(|value| value.checked_sub(1)) {
                    let index = index as usize;
                    if let Some(action) = self.action_items(&session_id).get(index).copied() {
                        return self.run_session_action(session_id, action).await;
                    }
                }
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_delete_confirm_key(
        &mut self,
        key: KeyEvent,
        session_id: String,
        selected: usize,
    ) -> Result<Option<Option<String>>> {
        let options = [ConfirmAction::Yes, ConfirmAction::No];
        match key.code {
            KeyCode::Esc => {
                self.mode = PickerMode::SessionActions {
                    session_id: session_id.clone(),
                    selected: self.action_index(&session_id, SessionAction::Delete),
                };
            }
            KeyCode::Up => self.set_confirm_selection(&session_id, selected.saturating_sub(1)),
            KeyCode::Down => self.set_confirm_selection(&session_id, (selected + 1).min(1)),
            KeyCode::Enter => match options[selected] {
                ConfirmAction::Yes => {
                    self.remove_session(&session_id).await?;
                    self.mode = PickerMode::Browse;
                    self.toast = Some(format!("removed session {session_id}"));
                }
                ConfirmAction::No => {
                    self.mode = PickerMode::SessionActions {
                        session_id: session_id.clone(),
                        selected: self.action_index(&session_id, SessionAction::Delete),
                    };
                }
            },
            KeyCode::Char('y') if key.modifiers.is_empty() => {
                self.remove_session(&session_id).await?;
                self.mode = PickerMode::Browse;
                self.toast = Some(format!("removed session {session_id}"));
            }
            KeyCode::Char('n') if key.modifiers.is_empty() => {
                self.mode = PickerMode::SessionActions {
                    session_id: session_id.clone(),
                    selected: self.action_index(&session_id, SessionAction::Delete),
                };
            }
            _ => {}
        }
        Ok(None)
    }

    async fn handle_diff_view_key(
        &mut self,
        key: KeyEvent,
        session_id: String,
    ) -> Result<Option<Option<String>>> {
        match key.code {
            KeyCode::Esc => {
                self.mode = PickerMode::SessionActions {
                    session_id: session_id.clone(),
                    selected: self.action_index(&session_id, SessionAction::Diff),
                };
            }
            KeyCode::Up => {
                self.detail_scroll = self.detail_scroll.saturating_sub(1);
            }
            KeyCode::Down => {
                self.detail_scroll = self.detail_scroll.saturating_add(1);
            }
            KeyCode::PageUp => {
                self.detail_scroll = self.detail_scroll.saturating_sub(10);
            }
            KeyCode::PageDown => {
                self.detail_scroll = self.detail_scroll.saturating_add(10);
            }
            _ => {}
        }
        Ok(None)
    }

    fn render_lines(&self, width: usize, cursor_visible: bool) -> Vec<String> {
        let mut lines = vec![render_host_picker_title_line(width), String::new()];
        lines.extend(self.render_header_lines(width, cursor_visible));

        if matches!(self.mode, PickerMode::Browse) {
            for (index, row) in self.picker_rows().into_iter().take(8).enumerate() {
                let selected = index == self.composer.selected;
                match row {
                    PickerRow::Create => {
                        let label = if self.composer.query.trim().is_empty() {
                            "Create new session".to_string()
                        } else {
                            format!("Create new session: {}", self.composer.query.trim())
                        };
                        lines.push(render_host_picker_option_line(
                            &format!("+  {label}"),
                            width,
                            selected,
                        ));
                    }
                    PickerRow::Session(session_id) => {
                        if let Some(session) =
                            self.sessions.iter().find(|item| item.session_id == session_id)
                        {
                            lines.push(render_host_picker_session_row(session, width, selected));
                        }
                    }
                }
            }
            let visible_sessions = self
                .picker_rows()
                .into_iter()
                .take(8)
                .filter_map(|row| match row {
                    PickerRow::Session(session_id) => {
                        self.sessions.iter().find(|item| item.session_id == session_id).cloned()
                    }
                    PickerRow::Create => None,
                })
                .collect::<Vec<_>>();
            let legend = render_host_picker_legend_row(width, &visible_sessions);
            if !legend.is_empty() {
                lines.push(String::new());
                lines.push(legend);
            }
        }

        if let Some(toast) = &self.toast {
            lines.push(String::new());
            lines.push(fit_host_picker_line(format!("notice: {toast}"), width));
        }

        lines
    }

    fn render_header_lines(&self, width: usize, cursor_visible: bool) -> Vec<String> {
        match &self.mode {
            PickerMode::Browse => vec![
                style_host_picker_background_row(width),
                style_host_picker_query_line(&self.composer.query, width, cursor_visible),
                style_host_picker_background_row(width),
                String::new(),
            ],
            PickerMode::CreateAgentSelect { selected } => {
                self.render_create_agent_lines(width, *selected)
            }
            PickerMode::SessionActions { session_id, selected } => {
                self.render_session_action_lines(width, session_id, *selected)
            }
            PickerMode::DiffView { session_id } => self.render_diff_lines(width, session_id),
            PickerMode::DeleteConfirm { session_id, selected } => {
                self.render_delete_confirm_lines(width, session_id, *selected)
            }
        }
    }

    fn render_session_action_lines(
        &self,
        width: usize,
        session_id: &str,
        selected: usize,
    ) -> Vec<String> {
        let mut lines = vec![style_host_picker_background_row(width)];
        let title = self
            .session_by_id(session_id)
            .map(|session| format!("{}  {}", session.title, session.branch))
            .unwrap_or_else(|| session_id.to_string());
        lines.push(style_host_picker_menu_line(&fit_host_picker_line(title, width), width, false));
        if let Some(session) = self.session_by_id(session_id) {
            lines.push(style_host_picker_menu_line(
                &fit_host_picker_line(session.worktree.clone(), width),
                width,
                false,
            ));
        }
        for (index, action) in self.action_items(session_id).into_iter().enumerate() {
            let label = format!("{}. {}", index + 1, action.label());
            lines.push(style_host_picker_menu_line(&label, width, index == selected));
        }
        lines.push(style_host_picker_background_row(width));
        lines.push(fit_host_picker_line("Enter selects. Esc goes back.".to_string(), width));
        lines.push(String::new());
        lines
    }

    fn render_create_agent_lines(&self, width: usize, selected: usize) -> Vec<String> {
        let mut lines = vec![style_host_picker_background_row(width)];
        let title = if self.composer.query.trim().is_empty() {
            "Choose coding agent".to_string()
        } else {
            format!("Create: {}", self.composer.query.trim())
        };
        lines.push(style_host_picker_menu_line(&fit_host_picker_line(title, width), width, false));
        for (index, agent) in self.create_agents.iter().enumerate() {
            let label = format!("{}. {}", index + 1, agent);
            lines.push(style_host_picker_menu_line(&label, width, index == selected));
        }
        lines.push(style_host_picker_background_row(width));
        lines.push(fit_host_picker_line("Enter selects. Esc goes back.".to_string(), width));
        lines.push(String::new());
        lines
    }

    fn render_delete_confirm_lines(
        &self,
        width: usize,
        session_id: &str,
        selected: usize,
    ) -> Vec<String> {
        let mut lines = vec![style_host_picker_background_row(width)];
        if let Some(session) = self.session_by_id(session_id) {
            lines.push(style_host_picker_menu_line(
                &fit_host_picker_line(session.worktree.clone(), width),
                width,
                false,
            ));
        }
        lines.push(style_host_picker_menu_line(
            &format!("Delete {session_id} and remove its worktree?"),
            width,
            false,
        ));
        for (index, action) in [ConfirmAction::Yes, ConfirmAction::No].into_iter().enumerate() {
            let label = format!(
                "{}. {}",
                index + 1,
                match action {
                    ConfirmAction::Yes => "yes",
                    ConfirmAction::No => "no",
                }
            );
            lines.push(style_host_picker_menu_line(&label, width, index == selected));
        }
        lines.push(style_host_picker_background_row(width));
        lines.push(fit_host_picker_line(
            "Enter selects. Esc returns to actions.".to_string(),
            width,
        ));
        lines.push(String::new());
        lines
    }

    fn render_diff_lines(&self, width: usize, session_id: &str) -> Vec<String> {
        let mut lines = vec![style_host_picker_background_row(width)];
        let title = self
            .session_by_id(session_id)
            .map(|session| format!("Diff: {}  {}", session.session_id, session.branch))
            .unwrap_or_else(|| format!("Diff: {session_id}"));
        lines.push(style_host_picker_menu_line(&fit_host_picker_line(title, width), width, false));
        if let Some(session) = self.session_by_id(session_id) {
            lines.push(style_host_picker_menu_line(
                &fit_host_picker_line(session.worktree.clone(), width),
                width,
                false,
            ));
        }
        let max_lines = 12usize;
        let detail_lines = self.detail_text.lines().collect::<Vec<_>>();
        let start = self.detail_scroll.min(detail_lines.len().saturating_sub(1));
        for line in detail_lines.into_iter().skip(start).take(max_lines) {
            lines.push(style_host_picker_diff_line(line, width));
        }
        if self.detail_text.is_empty() {
            lines.push(fit_host_picker_line("No diff available.".to_string(), width));
        }
        lines.push(style_host_picker_background_row(width));
        lines.push(fit_host_picker_line("Up/Down scroll. Esc goes back.".to_string(), width));
        lines.push(String::new());
        lines
    }

    fn filtered_sessions(&self) -> Vec<&SessionRecord> {
        let mut sessions = self
            .sessions
            .iter()
            .filter(|session| matches_query(session_search_text(session), &self.composer.query))
            .collect::<Vec<_>>();
        sessions.sort_by(|left, right| {
            session_rank(left)
                .cmp(&session_rank(right))
                .then_with(|| right.updated_at.cmp(&left.updated_at))
        });
        sessions
    }

    fn picker_rows(&self) -> Vec<PickerRow> {
        let mut rows = vec![PickerRow::Create];
        rows.extend(
            self.filtered_sessions()
                .into_iter()
                .map(|session| PickerRow::Session(session.session_id.clone())),
        );
        rows
    }

    fn clamp_selection(&mut self) {
        let len = self.picker_rows().len();
        if len == 0 {
            self.composer.selected = 0;
        } else {
            self.composer.selected = self.composer.selected.min(len - 1);
        }
    }

    fn clamp_mode_selection(&mut self) {
        self.mode = match self.mode.clone() {
            PickerMode::Browse => PickerMode::Browse,
            PickerMode::CreateAgentSelect { selected } => {
                let len = self.create_agents.len();
                if len == 0 {
                    PickerMode::Browse
                } else {
                    PickerMode::CreateAgentSelect { selected: selected.min(len - 1) }
                }
            }
            PickerMode::SessionActions { session_id, selected } => {
                let len = self.action_items(&session_id).len();
                if len == 0 || self.session_by_id(&session_id).is_none() {
                    PickerMode::Browse
                } else {
                    PickerMode::SessionActions { session_id, selected: selected.min(len - 1) }
                }
            }
            PickerMode::DiffView { session_id } => {
                if self.session_by_id(&session_id).is_none() {
                    PickerMode::Browse
                } else {
                    PickerMode::DiffView { session_id }
                }
            }
            PickerMode::DeleteConfirm { session_id, selected } => {
                if self.session_by_id(&session_id).is_none() {
                    PickerMode::Browse
                } else {
                    PickerMode::DeleteConfirm { session_id, selected: selected.min(1) }
                }
            }
        };
    }

    fn session_by_id(&self, session_id: &str) -> Option<&SessionRecord> {
        self.sessions.iter().find(|session| session.session_id == session_id)
    }

    fn refresh_agent_names(&mut self) -> Result<()> {
        self.create_agents = configured_agent_names(&self.paths)?;
        if self.create_agents.is_empty() {
            self.toast = Some("no configured agents found; falling back to codex".to_string());
            self.create_agents = vec!["codex".to_string()];
        }
        Ok(())
    }

    fn open_create_agent_menu(&mut self) {
        self.toast = None;
        self.mode =
            PickerMode::CreateAgentSelect { selected: self.default_create_agent_selection() };
        self.clamp_mode_selection();
    }

    fn open_action_menu(&mut self, session_id: &str) {
        self.toast = None;
        self.mode = PickerMode::SessionActions { session_id: session_id.to_string(), selected: 0 };
        self.clamp_mode_selection();
    }

    fn open_delete_confirmation(&mut self, session_id: &str, selected: usize) {
        self.toast = None;
        self.mode = PickerMode::DeleteConfirm { session_id: session_id.to_string(), selected };
        self.clamp_mode_selection();
    }

    fn action_items(&self, session_id: &str) -> Vec<SessionAction> {
        let Some(session) = self.session_by_id(session_id) else {
            return Vec::new();
        };
        let mut actions = Vec::new();
        if matches!(session.status, SessionStatus::Running | SessionStatus::NeedsInput) {
            actions.push(SessionAction::Attach);
        }
        actions.push(SessionAction::Diff);
        if session.merge_status == MergeStatus::Ready {
            actions.push(SessionAction::Merge);
        }
        actions.push(SessionAction::Delete);
        actions
    }

    fn set_action_selection(&mut self, session_id: &str, selected: usize) {
        self.mode = PickerMode::SessionActions { session_id: session_id.to_string(), selected };
        self.clamp_mode_selection();
    }

    fn set_create_agent_selection(&mut self, selected: usize) {
        self.mode = PickerMode::CreateAgentSelect { selected };
        self.clamp_mode_selection();
    }

    fn set_confirm_selection(&mut self, session_id: &str, selected: usize) {
        self.mode = PickerMode::DeleteConfirm { session_id: session_id.to_string(), selected };
        self.clamp_mode_selection();
    }

    fn default_create_agent_selection(&self) -> usize {
        let default_agent =
            configured_default_agent(&self.paths).unwrap_or_else(|_| "codex".to_string());
        self.create_agents.iter().position(|agent| agent == &default_agent).unwrap_or(0)
    }

    fn action_index(&self, session_id: &str, action: SessionAction) -> usize {
        self.action_items(session_id).iter().position(|item| *item == action).unwrap_or(0)
    }

    async fn run_session_action(
        &mut self,
        session_id: String,
        action: SessionAction,
    ) -> Result<Option<Option<String>>> {
        match action {
            SessionAction::Attach => Ok(Some(Some(session_id))),
            SessionAction::Diff => {
                self.load_diff(&session_id).await?;
                self.mode = PickerMode::DiffView { session_id };
                Ok(None)
            }
            SessionAction::Merge => {
                self.apply_session(&session_id).await?;
                self.mode = PickerMode::Browse;
                Ok(None)
            }
            SessionAction::Delete => {
                self.open_delete_confirmation(&session_id, 1);
                Ok(None)
            }
        }
    }

    async fn create_session_with_agent(&mut self, agent: &str) -> Result<Option<Option<String>>> {
        let title = self.composer.query.trim();
        let workspace = std::env::current_dir().context("failed to determine current directory")?;
        let response = send_request(
            &self.paths,
            &Request::CreateSession {
                workspace: workspace.to_string_lossy().to_string(),
                title: (!title.is_empty()).then(|| title.to_string()),
                agent: agent.to_string(),
                model: if agent == "codex" { Some(CODEX_MODELS[0].to_string()) } else { None },
                integration_policy: default_integration_policy(&self.paths),
            },
        )
        .await?;
        match response {
            Response::CreateSession { session } => Ok(Some(Some(session.session_id))),
            Response::Error { message } => {
                self.toast = Some(message);
                Ok(None)
            }
            other => bail!("unexpected response: {:?}", other),
        }
    }

    async fn apply_session(&mut self, session_id: &str) -> Result<()> {
        let response = send_request(
            &self.paths,
            &Request::ApplySession { session_id: session_id.to_string() },
        )
        .await?;
        match response {
            Response::Session { session } => {
                self.refresh_sessions().await?;
                self.toast = Some(format!("merged {}", session.session_id));
                Ok(())
            }
            Response::Error { message } => {
                self.toast = Some(message);
                Ok(())
            }
            other => bail!("unexpected response: {:?}", other),
        }
    }

    async fn remove_session(&mut self, session_id: &str) -> Result<()> {
        let response = send_request(
            &self.paths,
            &Request::KillSession { session_id: session_id.to_string(), remove: true },
        )
        .await?;
        match response {
            Response::KillSession { .. } => {
                self.refresh_sessions().await?;
                Ok(())
            }
            Response::Error { message } => bail!(message),
            other => bail!("unexpected response: {:?}", other),
        }
    }

    async fn load_diff(&mut self, session_id: &str) -> Result<()> {
        let response =
            send_request(&self.paths, &Request::DiffSession { session_id: session_id.to_string() })
                .await?;
        match response {
            Response::Diff { diff } => {
                self.detail_text = diff.diff;
                self.detail_scroll = 0;
                Ok(())
            }
            Response::Error { message } => {
                self.toast = Some(message);
                Ok(())
            }
            other => bail!("unexpected response: {:?}", other),
        }
    }
}

fn fit_host_picker_line(mut line: String, width: usize) -> String {
    if width == 0 {
        return String::new();
    }
    let max_chars = width.saturating_sub(1).max(1);
    let char_count = line.chars().count();
    if char_count <= max_chars {
        return line;
    }
    line = line.chars().take(max_chars).collect();
    line
}

fn style_host_picker_background_row(width: usize) -> String {
    let max_chars = width.saturating_sub(1).max(1);
    format!("{HOST_PICKER_QUERY_BG}{}{ANSI_RESET}", " ".repeat(max_chars))
}

fn render_host_picker_title_line(width: usize) -> String {
    let title = format!("{} - {}", render_host_picker_brand(), render_host_picker_subtitle());
    fit_host_picker_line(title, width)
}

fn render_host_picker_brand() -> String {
    "agentd".to_string()
}

fn render_host_picker_subtitle() -> String {
    render_ansi_gradient("agent multiplexer", (162, 96, 252), (104, 250, 253))
}

fn render_host_picker_gradient_text(text: &str) -> String {
    const START: u8 = 54;
    const END: u8 = 159;
    let chars = text.chars().collect::<Vec<_>>();
    let last_index = chars.len().saturating_sub(1);
    let mut rendered = String::new();
    for (index, ch) in chars.into_iter().enumerate() {
        let value = interpolate_ansi_value(START, END, index, last_index);
        rendered.push_str(&format!("{}", ch.to_string().with(CrosColor::AnsiValue(value))));
    }
    rendered
}

pub fn render_ansi_gradient(text: &str, start: (u8, u8, u8), end: (u8, u8, u8)) -> String {
    let chars: Vec<char> = text.chars().collect();
    let len = chars.len();

    if len == 0 {
        return String::new();
    }

    chars
        .iter()
        .enumerate()
        .map(|(i, ch)| {
            let t = if len == 1 { 0.0 } else { i as f32 / (len - 1) as f32 };

            let r = start.0 as f32 + (end.0 as f32 - start.0 as f32) * t;
            let g = start.1 as f32 + (end.1 as f32 - start.1 as f32) * t;
            let b = start.2 as f32 + (end.2 as f32 - start.2 as f32) * t;

            format!("\x1b[38;2;{};{};{}m{}", r as u8, g as u8, b as u8, ch)
        })
        .collect::<String>()
        + "\x1b[0m"
}

fn interpolate_ansi_value(start: u8, end: u8, index: usize, last_index: usize) -> u8 {
    if last_index == 0 {
        return start;
    }

    let start = start as f32;
    let end = end as f32;
    let t = index as f32 / last_index as f32;
    (start + ((end - start) * t)).round() as u8
}

fn render_host_picker_option_line(content: &str, width: usize, selected: bool) -> String {
    let label = if selected {
        format!("{HOST_PICKER_SELECTED_STYLE}› {content}{ANSI_RESET}")
    } else {
        format!("  {content}")
    };
    fit_host_picker_line(label, width)
}

fn render_session_list_header_row(layout: SessionListLayout) -> String {
    style_host_picker_content_row(&render_session_list_header_content(layout), layout.visible_width)
}

fn render_session_list_row(session: &SessionRecord, layout: SessionListLayout) -> String {
    let status = render_session_list_status_cell(session, layout.status);
    let age = format_session_list_cell(&session_elapsed_label(session), layout.age);
    let session_id = format_session_list_cell(&session.session_id, layout.session);
    let title = format_session_list_cell(&session.title, layout.title);
    let repo = format_session_list_cell(&session.repo_name, layout.repo);
    let branch = format_session_list_cell(&session.branch, layout.branch);
    format!("  {status}  {age}  {session_id}  {title}  {repo}  {branch}")
}

fn render_session_list_header_content(layout: SessionListLayout) -> String {
    format!(
        "  {}  {}  {}  {}  {}  {}",
        format_session_list_cell("STATUS", layout.status),
        format_session_list_cell("AGE", layout.age),
        format_session_list_cell("SESSION", layout.session),
        format_session_list_cell("TITLE", layout.title),
        format_session_list_cell("REPO", layout.repo),
        format_session_list_cell("BRANCH", layout.branch),
    )
}

fn render_session_list_status_cell(session: &SessionRecord, width: usize) -> String {
    let icon = session_icon(session);
    let label = session_list_status_label(session);
    let plain = format!("{icon} {label}");
    let padding = " ".repeat(width.saturating_sub(plain.chars().count()));
    format!("{}{icon}{ANSI_RESET} {label}{padding}", host_picker_icon_ansi_prefix(session))
}

fn style_host_picker_content_row(content: &str, visible_width: usize) -> String {
    let visible = content.chars().take(visible_width).collect::<String>();
    format!("{HOST_PICKER_QUERY_BG}{HOST_PICKER_TEXT_FG}{visible}{ANSI_RESET}")
}

fn session_list_layout(width: usize) -> SessionListLayout {
    let visible_width = session_list_visible_width(width);
    let mut layout = SessionListLayout {
        visible_width,
        status: SESSION_LIST_STATUS_WIDTH,
        age: SESSION_LIST_AGE_WIDTH,
        session: SESSION_LIST_SESSION_MIN_WIDTH,
        title: SESSION_LIST_TITLE_MIN_WIDTH,
        repo: SESSION_LIST_REPO_MIN_WIDTH,
        branch: SESSION_LIST_BRANCH_MIN_WIDTH,
    };
    let min_total = session_list_total_width(layout);
    if visible_width >= min_total {
        let mut remaining = visible_width - min_total;
        grow_session_list_column(&mut layout.branch, SESSION_LIST_BRANCH_MAX_WIDTH, &mut remaining);
        grow_session_list_column(&mut layout.title, SESSION_LIST_TITLE_MAX_WIDTH, &mut remaining);
        grow_session_list_column(
            &mut layout.session,
            SESSION_LIST_SESSION_MAX_WIDTH,
            &mut remaining,
        );
        grow_session_list_column(&mut layout.repo, SESSION_LIST_REPO_MAX_WIDTH, &mut remaining);
        return layout;
    }

    let mut deficit = min_total - visible_width;
    shrink_session_list_column(&mut layout.repo, SESSION_LIST_REPO_FLOOR_WIDTH, &mut deficit);
    shrink_session_list_column(&mut layout.session, SESSION_LIST_SESSION_FLOOR_WIDTH, &mut deficit);
    shrink_session_list_column(&mut layout.title, SESSION_LIST_TITLE_FLOOR_WIDTH, &mut deficit);
    shrink_session_list_column(&mut layout.branch, SESSION_LIST_BRANCH_FLOOR_WIDTH, &mut deficit);
    layout
}

fn session_list_visible_width(width: usize) -> usize {
    width.max(1).min(SESSION_LIST_DEFAULT_WIDTH)
}

fn session_list_total_width(layout: SessionListLayout) -> usize {
    SESSION_LIST_STRUCTURAL_WIDTH
        + layout.status
        + layout.age
        + layout.session
        + layout.title
        + layout.repo
        + layout.branch
}

fn grow_session_list_column(width: &mut usize, max_width: usize, remaining: &mut usize) {
    let add = (*remaining).min(max_width.saturating_sub(*width));
    *width += add;
    *remaining -= add;
}

fn shrink_session_list_column(width: &mut usize, floor_width: usize, deficit: &mut usize) {
    let remove = (*deficit).min(width.saturating_sub(floor_width));
    *width -= remove;
    *deficit -= remove;
}

fn format_session_list_cell(content: &str, width: usize) -> String {
    let truncated = truncate_session_list_cell(content, width);
    format!("{truncated:<width$}")
}

fn truncate_session_list_cell(content: &str, width: usize) -> String {
    let char_count = content.chars().count();
    if char_count <= width {
        return content.to_string();
    }
    if width <= 3 {
        return content.chars().take(width).collect();
    }
    let mut truncated = content.chars().take(width - 3).collect::<String>();
    truncated.push_str("...");
    truncated
}

fn style_host_picker_menu_line(content: &str, width: usize, selected: bool) -> String {
    let max_chars = width.saturating_sub(1).max(1);
    let prefix = if selected { "› " } else { "  " };
    let plain = format!("{prefix}{content}");
    let visible = plain.chars().take(max_chars).collect::<String>();
    let visible_len = visible.chars().count();
    let padding = max_chars.saturating_sub(visible_len);
    let style = if selected { HOST_PICKER_SELECTED_STYLE } else { "" };
    format!("{HOST_PICKER_QUERY_BG}{style}{visible}{}{ANSI_RESET}", " ".repeat(padding))
}

fn style_host_picker_diff_line(content: &str, width: usize) -> String {
    let visible = fit_host_picker_line(content.to_string(), width);
    let style = if visible.starts_with("diff --git")
        || visible.starts_with("--- ")
        || visible.starts_with("+++ ")
    {
        HOST_PICKER_DIFF_HEADER_STYLE
    } else if visible.starts_with("@@") {
        HOST_PICKER_DIFF_HUNK_STYLE
    } else if visible.starts_with('+') && !visible.starts_with("+++") {
        HOST_PICKER_DIFF_ADD_STYLE
    } else if visible.starts_with('-') && !visible.starts_with("---") {
        HOST_PICKER_DIFF_REMOVE_STYLE
    } else {
        ""
    };
    if style.is_empty() { visible } else { format!("{style}{visible}{ANSI_RESET}") }
}

fn style_host_picker_query_line(query: &str, width: usize, cursor_visible: bool) -> String {
    let max_chars = width.saturating_sub(1).max(1);
    let marker = "› ";
    let marker_len = marker.chars().count();
    let cursor = if cursor_visible { HOST_PICKER_CURSOR } else { " " };
    let cursor_len = 1;
    let available = max_chars.saturating_sub(marker_len + cursor_len);

    let (content, color, cursor_before_content) = if query.is_empty() {
        ("Type to filter sessions or name a new one.".to_string(), HOST_PICKER_PLACEHOLDER_FG, true)
    } else {
        (query.to_string(), HOST_PICKER_TEXT_FG, false)
    };
    let content_budget =
        if cursor_before_content { available } else { available.saturating_sub(cursor_len) };
    let visible = fit_host_picker_line(content, content_budget.saturating_add(1));
    let visible = visible.chars().take(content_budget).collect::<String>();
    let visible_len = visible.chars().count();
    let padding = max_chars.saturating_sub(marker_len + visible_len + cursor_len);
    let content_with_cursor = if cursor_before_content {
        format!("{HOST_PICKER_TEXT_FG}{cursor}{color}{visible}")
    } else {
        format!("{color}{visible}{HOST_PICKER_TEXT_FG}{cursor}")
    };

    format!(
        "{HOST_PICKER_QUERY_BG}{HOST_PICKER_TEXT_FG}{marker}{content_with_cursor}{}{ANSI_RESET}",
        " ".repeat(padding)
    )
}

impl SessionAction {
    fn label(self) -> &'static str {
        match self {
            Self::Attach => "attach",
            Self::Diff => "diff",
            Self::Merge => "merge",
            Self::Delete => "delete",
        }
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
                return Ok(Some(OverlayOutcome::ForwardInput(vec![0x02])));
            }
            KeyCode::Enter => {
                if let Some(item) = items.get(self.palette_selected) {
                    return self.run_command(item.command).await;
                }
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
                    return Ok(Some(OverlayOutcome::SwitchSession(session.session_id.clone())));
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
                        integration_policy: default_integration_policy(&self.paths),
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
                let sync = session.merge_status.as_str();
                let summary = session
                    .merge_summary
                    .clone()
                    .unwrap_or_else(|| "merge status unavailable".to_string());
                self.detail_text = format!(
                    "repo      {}\nrepo_path  {}\nworktree   {}\nbranch     {}\nbase       {}\nmerge     {}\nconflicts  {}\nstatus     {}\n\n{}",
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
                    &Request::DiffSession { session_id: self.session_id.clone() },
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
                    Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
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
                    Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
                } else {
                    Style::default()
                };
                Line::from(vec![
                    Span::styled(session_icon(session), session_icon_style(session)),
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
        let title_style =
            if edit_agent { Style::default() } else { Style::default().fg(Color::Cyan) };
        let agent_style =
            if edit_agent { Style::default().fg(Color::Cyan) } else { Style::default() };
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
        PaletteItem { key_hint: "s", title: "Switch Session", command: Command::SessionSwitcher },
        PaletteItem { key_hint: "t", title: "New Session", command: Command::NewSession },
        PaletteItem { key_hint: "g", title: "Session Details", command: Command::GitStatus },
        PaletteItem { key_hint: "d", title: "Diff", command: Command::Diff },
        PaletteItem { key_hint: "x", title: "Stop Session", command: Command::StopSession },
    ]
}

fn session_search_text(session: &SessionRecord) -> String {
    format!(
        "{} {} {} {} {} {}",
        session.title,
        session.repo_name,
        session.branch,
        session.status_string(),
        session.apply_state.as_str(),
        session.attention_string()
    )
}

fn session_rank(session: &SessionRecord) -> u8 {
    if session.status == SessionStatus::NeedsInput {
        0
    } else if matches!(session.status, SessionStatus::Failed | SessionStatus::UnknownRecovered)
        || session.has_conflicts
    {
        1
    } else if session.merge_status == MergeStatus::Conflicted {
        2
    } else if matches!(session.merge_status, MergeStatus::Ready | MergeStatus::Blocked) {
        3
    } else if matches!(session.status, SessionStatus::Running | SessionStatus::Creating) {
        4
    } else {
        5
    }
}

fn session_icon(session: &SessionRecord) -> &'static str {
    match session.merge_status {
        MergeStatus::Ready => "⧖",
        MergeStatus::Blocked => "⚠",
        MergeStatus::Conflicted => "✖",
        _ => match session.status {
            SessionStatus::NeedsInput => "◦",
            SessionStatus::Failed => "✖",
            SessionStatus::UnknownRecovered => "⚠",
            SessionStatus::Running | SessionStatus::Creating => "●",
            SessionStatus::Exited => "✔",
        },
    }
}

fn session_icon_color(session: &SessionRecord) -> Color {
    match session.merge_status {
        MergeStatus::Ready => Color::Blue,
        MergeStatus::Blocked => Color::Yellow,
        MergeStatus::Conflicted => Color::Red,
        _ => match session.status {
            SessionStatus::NeedsInput => Color::Yellow,
            SessionStatus::Failed => Color::Red,
            SessionStatus::UnknownRecovered => Color::Yellow,
            SessionStatus::Running | SessionStatus::Creating => Color::Green,
            SessionStatus::Exited => Color::Green,
        },
    }
}

fn session_icon_style(session: &SessionRecord) -> Style {
    let mut style = Style::default().fg(session_icon_color(session));
    if session.status == SessionStatus::NeedsInput {
        style = style.add_modifier(Modifier::DIM);
    }
    style
}

fn render_host_picker_session_row(session: &SessionRecord, width: usize, selected: bool) -> String {
    let max_chars = width.saturating_sub(1).max(1);
    let leader = if selected { "› ".to_string() } else { "  ".to_string() };
    let separator = "  ";
    let elapsed = session_elapsed_label(session);
    let tail = format!(
        "{}  {}  {}  {}  {}",
        elapsed, session.session_id, session.title, session.repo_name, session.branch
    );
    let used = leader.chars().count() + session_icon(session).chars().count() + separator.len();
    let remaining = max_chars.saturating_sub(used);
    let visible_tail = tail.chars().take(remaining).collect::<String>();
    let tail_style = if selected { HOST_PICKER_SELECTED_STYLE } else { "" };
    format!(
        "{tail_style}{leader}{ANSI_RESET}{}{icon}{ANSI_RESET}{separator}{tail_style}{visible_tail}{ANSI_RESET}",
        host_picker_icon_ansi_prefix(session),
        icon = session_icon(session),
    )
}

fn render_host_picker_legend_row(width: usize, sessions: &[SessionRecord]) -> String {
    let entries = [
        (demo_status_session(SessionStatus::NeedsInput, ApplyState::Idle), "needs input"),
        (demo_status_session(SessionStatus::Failed, ApplyState::Idle), "failed"),
        (demo_status_session(SessionStatus::UnknownRecovered, ApplyState::Idle), "recovered"),
        (
            {
                let mut session = demo_status_session(SessionStatus::Exited, ApplyState::Idle);
                session.merge_status = MergeStatus::Ready;
                session
            },
            "ready to merge",
        ),
        (
            {
                let mut session = demo_status_session(SessionStatus::Exited, ApplyState::Idle);
                session.merge_status = MergeStatus::Blocked;
                session
            },
            "merge blocked",
        ),
        (demo_status_session(SessionStatus::Running, ApplyState::Idle), "running"),
        (demo_status_session(SessionStatus::UnknownRecovered, ApplyState::Idle), "recovered"),
        (demo_status_session(SessionStatus::Exited, ApplyState::Idle), "exited"),
    ];
    let max_chars = width.saturating_sub(1).max(1);
    let mut line = "  ".to_string();
    let mut matching_entries = entries
        .into_iter()
        .filter(|(entry_session, _)| {
            sessions.iter().any(|session| session_icon(session) == session_icon(entry_session))
        })
        .peekable();
    if matching_entries.peek().is_none() {
        return String::new();
    }
    let mut visible = 0usize;
    for (index, (session, label)) in matching_entries.enumerate() {
        let separator = if index > 0 {
            format!("{HOST_PICKER_LEGEND_TEXT_STYLE} • {ANSI_RESET}")
        } else {
            String::new()
        };
        let plain_entry = format!("{} {}", session_icon(&session), label);
        let needed = if index > 0 { 3 } else { 0 } + plain_entry.chars().count();
        if visible > 0 && visible + needed > max_chars {
            break;
        }
        if visible == 0 && plain_entry.chars().count() > max_chars {
            return String::new();
        }
        if index > 0 {
            line.push_str(&separator);
            visible += 3;
        }
        line.push_str(&format!(
            "{}{}{ANSI_RESET}{HOST_PICKER_LEGEND_TEXT_STYLE} {label}{ANSI_RESET}",
            host_picker_icon_ansi_prefix(&session),
            session_icon(&session),
        ));
        visible += plain_entry.chars().count();
    }
    line
}

fn demo_status_session(status: SessionStatus, apply_state: ApplyState) -> SessionRecord {
    SessionRecord {
        session_id: String::new(),
        thread_id: None,
        agent: "codex".to_string(),
        model: None,
        mode: SessionMode::Execute,
        workspace: String::new(),
        repo_path: String::new(),
        repo_name: String::new(),
        title: String::new(),
        base_branch: String::new(),
        branch: String::new(),
        worktree: String::new(),
        status,
        integration_policy: IntegrationPolicy::AutoApplySafe,
        apply_state,
        merge_status: MergeStatus::Unknown,
        merge_summary: None,
        has_conflicts: false,
        pid: None,
        exit_code: None,
        error: None,
        attention: agentd_shared::session::AttentionLevel::Info,
        attention_summary: None,
        created_at: chrono::Utc::now(),
        updated_at: chrono::Utc::now(),
        exited_at: None,
    }
}

fn session_list_status_label(session: &SessionRecord) -> &'static str {
    if session.merge_status == MergeStatus::Ready {
        "ready to merge"
    } else if session.merge_status == MergeStatus::Blocked {
        "merge blocked"
    } else if session.merge_status == MergeStatus::Conflicted {
        "merge conflicted"
    } else if session.apply_state == ApplyState::Applied {
        "applied"
    } else {
        match session.status {
            SessionStatus::NeedsInput => "needs input",
            SessionStatus::Failed => "failed",
            SessionStatus::UnknownRecovered => "recovered",
            SessionStatus::Running | SessionStatus::Creating => "running",
            SessionStatus::Exited => "exited",
        }
    }
}

fn host_picker_icon_ansi_prefix(session: &SessionRecord) -> String {
    match session.status {
        SessionStatus::NeedsInput => format!("{ANSI_DIM}{HOST_PICKER_STATUS_YELLOW_FG}"),
        SessionStatus::Failed => HOST_PICKER_STATUS_RED_FG.to_string(),
        SessionStatus::UnknownRecovered => HOST_PICKER_STATUS_YELLOW_FG.to_string(),
        SessionStatus::Running | SessionStatus::Creating => HOST_PICKER_STATUS_GREEN_FG.to_string(),
        SessionStatus::Exited => match session.merge_status {
            MergeStatus::Ready => HOST_PICKER_STATUS_BLUE_FG.to_string(),
            MergeStatus::Blocked => HOST_PICKER_STATUS_YELLOW_FG.to_string(),
            MergeStatus::Conflicted => HOST_PICKER_STATUS_RED_FG.to_string(),
            _ => HOST_PICKER_STATUS_GREEN_FG.to_string(),
        },
    }
}

fn session_status_text(session: &SessionRecord) -> String {
    if let Some(summary) = &session.attention_summary {
        return summary.clone();
    }
    if session.apply_state == ApplyState::Applied {
        return "applied".to_string();
    }
    if let Some(summary) = &session.merge_summary {
        return summary.clone();
    }
    match session.status {
        SessionStatus::Creating => "starting".to_string(),
        SessionStatus::Running => "running".to_string(),
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

#[cfg(test)]
mod tests {
    use super::{
        ANSI_RESET, AttachOverlay, HOST_PICKER_CURSOR, HOST_PICKER_DIFF_ADD_STYLE,
        HOST_PICKER_DIFF_HEADER_STYLE, HOST_PICKER_DIFF_HUNK_STYLE, HOST_PICKER_DIFF_REMOVE_STYLE,
        HOST_PICKER_ENTER_SEQUENCE, HOST_PICKER_EXIT_SEQUENCE, HOST_PICKER_LEGEND_TEXT_STYLE,
        HOST_PICKER_PLACEHOLDER_FG, HOST_PICKER_QUERY_BG, HOST_PICKER_SELECTED_STYLE,
        HOST_PICKER_STATUS_BLUE_FG, HOST_PICKER_STATUS_GREEN_FG, HOST_PICKER_STATUS_RED_FG,
        HOST_PICKER_STATUS_YELLOW_FG, HOST_PICKER_TEXT_FG, OverlayMode, OverlayOutcome,
        PickerComposer, PickerMode, PickerRow, SessionAction, SessionPicker,
        configured_agent_names, fit_host_picker_line, interpolate_ansi_value,
        render_host_picker_brand, render_host_picker_gradient_text, render_host_picker_legend_row,
        render_host_picker_session_row, render_host_picker_subtitle, render_host_picker_title_line,
        render_session_list_header_content, render_session_list_header_row,
        render_session_list_lines, session_icon, session_icon_color, session_list_layout,
        style_host_picker_background_row, style_host_picker_diff_line,
        style_host_picker_query_line,
    };
    use agentd_shared::{
        paths::AppPaths,
        session::{
            ApplyState, AttentionLevel, IntegrationPolicy, MergeStatus, SessionMode, SessionRecord,
            SessionStatus,
        },
    };
    use camino::Utf8PathBuf;
    use chrono::{Duration, Utc};
    use crossterm::event::{Event, KeyCode, KeyEvent, KeyEventKind, KeyEventState, KeyModifiers};
    use futures::executor::block_on;

    #[derive(Clone, Copy)]
    enum IntegrationState {
        Clean,
        PendingReview,
        Blocked,
        Conflicted,
    }

    fn strip_ansi(input: &str) -> String {
        let mut output = String::new();
        let mut chars = input.chars().peekable();
        while let Some(ch) = chars.next() {
            if ch == '\u{1b}' {
                if matches!(chars.peek(), Some('[')) {
                    chars.next();
                    while let Some(next) = chars.next() {
                        if ('@'..='~').contains(&next) {
                            break;
                        }
                    }
                }
                continue;
            }
            output.push(ch);
        }
        output
    }

    #[test]
    fn picker_rows_include_create_and_matching_sessions() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha", "repo-a"), demo("beta", "repo-b")];

        let rows = picker.picker_rows();
        assert_eq!(rows[0], PickerRow::Create);
        assert_eq!(rows.len(), 3);
    }

    #[test]
    fn query_filter_keeps_create_row() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha", "repo-a"), demo("beta", "repo-b")];
        picker.composer = PickerComposer { query: "beta".to_string(), selected: 0 };

        let rows = picker.picker_rows();
        assert_eq!(rows, vec![PickerRow::Create, PickerRow::Session("beta".to_string())]);
    }

    #[test]
    fn clamp_selection_caps_to_available_rows() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha", "repo-a")];
        picker.composer.selected = 8;

        picker.clamp_selection();

        assert_eq!(picker.composer.selected, 1);
    }

    #[test]
    fn attach_overlay_ctrl_b_forwards_literal_byte() {
        let mut overlay = AttachOverlay::new(test_paths(), "alpha".to_string());
        overlay.mode = OverlayMode::Palette;

        let outcome =
            block_on(overlay.handle_key(KeyEvent::new(KeyCode::Char('b'), KeyModifiers::CONTROL)))
                .unwrap();

        assert_eq!(outcome, Some(OverlayOutcome::ForwardInput(vec![0x02])));
    }

    #[test]
    fn attach_overlay_escape_closes_palette() {
        let mut overlay = AttachOverlay::new(test_paths(), "alpha".to_string());
        overlay.mode = OverlayMode::Palette;

        let outcome = block_on(overlay.handle_event(Event::Key(KeyEvent {
            code: KeyCode::Esc,
            modifiers: KeyModifiers::NONE,
            kind: KeyEventKind::Press,
            state: KeyEventState::NONE,
        })))
        .unwrap();

        assert_eq!(outcome, Some(OverlayOutcome::Close));
    }

    #[test]
    fn action_menu_shows_attach_for_live_sessions() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        assert_eq!(
            picker.action_items("alpha"),
            vec![SessionAction::Attach, SessionAction::Diff, SessionAction::Delete]
        );
    }

    #[test]
    fn action_menu_hides_attach_for_exited_sessions() {
        let mut session = demo("alpha", "repo-a");
        session.status = SessionStatus::Exited;
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![session],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        assert_eq!(picker.action_items("alpha"), vec![SessionAction::Diff, SessionAction::Delete]);
    }

    #[test]
    fn action_menu_shows_merge_for_pending_review() {
        let mut session = demo("alpha", "repo-a");
        session.merge_status = MergeStatus::Ready;
        session.status = SessionStatus::Exited;
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![session],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        assert_eq!(
            picker.action_items("alpha"),
            vec![SessionAction::Diff, SessionAction::Merge, SessionAction::Delete]
        );
    }

    #[test]
    fn action_menu_shows_attach_and_merge_when_both_apply() {
        let mut session = demo("alpha", "repo-a");
        session.merge_status = MergeStatus::Ready;
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![session],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        assert_eq!(
            picker.action_items("alpha"),
            vec![
                SessionAction::Attach,
                SessionAction::Diff,
                SessionAction::Merge,
                SessionAction::Delete
            ]
        );
    }

    #[test]
    fn clamp_mode_selection_caps_to_visible_actions() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::SessionActions { session_id: "alpha".to_string(), selected: 9 },
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        picker.clamp_mode_selection();

        assert_eq!(
            picker.mode,
            PickerMode::SessionActions { session_id: "alpha".to_string(), selected: 2 }
        );
    }

    #[test]
    fn clamp_mode_selection_resets_when_session_disappears() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: Vec::new(),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::DeleteConfirm { session_id: "missing".to_string(), selected: 1 },
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        picker.clamp_mode_selection();

        assert_eq!(picker.mode, PickerMode::Browse);
    }

    #[test]
    fn render_lines_hides_session_list_in_action_mode() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a"), demo("beta", "repo-b")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::SessionActions { session_id: "alpha".to_string(), selected: 0 },
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        let lines = picker.render_lines(120, true);
        let rendered = lines.join("\n");

        assert!(!rendered.contains("title-beta"));
        assert!(rendered.contains("› 1. attach"));
        assert!(rendered.contains("  2. diff"));
        assert!(rendered.contains("/tmp/alpha"));
    }

    #[test]
    fn render_lines_show_selector_on_delete_confirmation_rows() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::DeleteConfirm { session_id: "alpha".to_string(), selected: 1 },
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        let lines = picker.render_lines(120, true);
        let rendered = lines.join("\n");

        assert!(rendered.contains("  1. yes"));
        assert!(rendered.contains("› 2. no"));
    }

    #[test]
    fn render_lines_remove_helper_and_agent_row_in_browse_mode() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        let rendered = picker.render_lines(120, true).join("\n");
        assert!(!rendered.contains("Tab switches field"));
        assert!(!rendered.contains("Enter opens the selected row"));
        assert!(!rendered.contains("agent  "));
        assert!(rendered.contains(HOST_PICKER_STATUS_GREEN_FG));
        assert!(rendered.contains("running"));
        assert!(rendered.contains("agentd -"));
        assert!(!rendered.contains("needs input"));
        assert!(!rendered.contains("pending review"));
    }

    #[test]
    fn create_agent_menu_defaults_to_codex_when_present() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["codex".to_string(), "claude".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        picker.open_create_agent_menu();

        assert_eq!(picker.mode, PickerMode::CreateAgentSelect { selected: 0 });
    }

    #[test]
    fn create_agent_menu_clamps_and_renders_agents() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["codex".to_string(), "claude".to_string()],
            composer: default_composer(),
            mode: PickerMode::CreateAgentSelect { selected: 9 },
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        picker.clamp_mode_selection();
        let rendered = picker.render_lines(120, true).join("\n");

        assert_eq!(picker.mode, PickerMode::CreateAgentSelect { selected: 1 });
        assert!(rendered.contains("1. codex"));
        assert!(rendered.contains("› 2. claude"));
        assert!(!rendered.contains("title-alpha"));
    }

    #[test]
    fn configured_agent_names_use_default_config_order() {
        let names = configured_agent_names(&test_paths()).unwrap();
        assert_eq!(names, vec!["codex".to_string(), "claude".to_string()]);
    }

    #[test]
    fn runtime_icons_match_requested_symbols() {
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::NeedsInput,
                IntegrationState::Clean
            )),
            "◦"
        );
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Failed,
                IntegrationState::Clean
            )),
            "✖"
        );
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::UnknownRecovered,
                IntegrationState::Clean
            )),
            "⚠"
        );
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Running,
                IntegrationState::PendingReview
            )),
            "⧖"
        );
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Running,
                IntegrationState::Clean
            )),
            "●"
        );
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::UnknownRecovered,
                IntegrationState::Clean
            )),
            "⚠"
        );
        assert_eq!(
            session_icon(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Exited,
                IntegrationState::Clean
            )),
            "✔"
        );
    }

    #[test]
    fn runtime_icon_colors_match_requested_palette() {
        assert_eq!(
            session_icon_color(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::NeedsInput,
                IntegrationState::Clean
            )),
            ratatui::style::Color::Yellow
        );
        assert_eq!(
            session_icon_color(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Failed,
                IntegrationState::Clean
            )),
            ratatui::style::Color::Red
        );
        assert_eq!(
            session_icon_color(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::UnknownRecovered,
                IntegrationState::Clean
            )),
            ratatui::style::Color::Yellow
        );
        assert_eq!(
            session_icon_color(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Exited,
                IntegrationState::PendingReview
            )),
            ratatui::style::Color::Blue
        );
        assert_eq!(
            session_icon_color(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::Running,
                IntegrationState::Clean
            )),
            ratatui::style::Color::Green
        );
        assert_eq!(
            session_icon_color(&demo_with(
                "alpha",
                "repo-a",
                SessionStatus::UnknownRecovered,
                IntegrationState::Clean
            )),
            ratatui::style::Color::Yellow
        );
    }

    #[test]
    fn host_picker_session_row_includes_status_color_escape() {
        let mut running_session =
            demo_with("alpha", "repo-a", SessionStatus::Running, IntegrationState::Clean);
        running_session.created_at = Utc::now() - Duration::minutes(23);
        let running = render_host_picker_session_row(&running_session, 120, true);
        let review = render_host_picker_session_row(
            &demo_with("alpha", "repo-a", SessionStatus::Exited, IntegrationState::PendingReview),
            120,
            false,
        );
        let failed = render_host_picker_session_row(
            &demo_with("alpha", "repo-a", SessionStatus::Failed, IntegrationState::Clean),
            120,
            false,
        );

        assert!(running.contains(HOST_PICKER_STATUS_GREEN_FG));
        assert!(review.contains(HOST_PICKER_STATUS_BLUE_FG));
        assert!(failed.contains(HOST_PICKER_STATUS_RED_FG));
        assert!(running.contains(HOST_PICKER_SELECTED_STYLE));
        assert!(running.contains("› "));
        assert!(running.contains("●"));
        assert!(running.contains("23m  alpha  title-alpha"));
        assert!(review.contains("⧖"));
        assert!(failed.contains("✖"));
        let needs_input = render_host_picker_session_row(
            &demo_with("alpha", "repo-a", SessionStatus::NeedsInput, IntegrationState::Clean),
            120,
            false,
        );
        assert!(needs_input.contains(HOST_PICKER_STATUS_YELLOW_FG));
    }

    #[test]
    fn host_picker_legend_uses_colored_icons_and_separator() {
        let rendered = render_host_picker_legend_row(
            200,
            &[
                demo_with("alpha", "repo-a", SessionStatus::NeedsInput, IntegrationState::Clean),
                demo_with("beta", "repo-b", SessionStatus::Exited, IntegrationState::PendingReview),
                demo_with(
                    "gamma",
                    "repo-c",
                    SessionStatus::UnknownRecovered,
                    IntegrationState::Clean,
                ),
            ],
        );
        assert!(rendered.contains(HOST_PICKER_STATUS_YELLOW_FG));
        assert!(rendered.contains(HOST_PICKER_STATUS_BLUE_FG));
        assert!(rendered.contains(HOST_PICKER_LEGEND_TEXT_STYLE));
        assert!(rendered.starts_with("  "));
        assert!(rendered.contains("◦"));
        assert!(rendered.contains("⧖"));
        assert!(rendered.contains("⚠"));
        assert!(rendered.contains("needs input"));
        assert!(rendered.contains("ready to merge"));
        assert!(rendered.contains("recovered"));
        assert!(rendered.contains(" • "));
        assert!(!rendered.contains("failed"));
        assert!(!rendered.contains("running"));
    }

    #[test]
    fn host_picker_title_renders_brand_gradient() {
        let brand = render_host_picker_brand();
        let subtitle = render_host_picker_subtitle();
        let title = render_host_picker_title_line(120);
        assert_eq!(brand, "agentd");
        assert_eq!(strip_ansi(&subtitle), "agent multiplexer");
        assert!(subtitle.contains('\u{1b}'));
        assert!(subtitle.contains('a'));
        assert!(subtitle.contains('r'));
        assert!(title.contains("agentd - "));
    }

    #[test]
    fn host_picker_gradient_progresses_without_repeating() {
        let last_index = "agent multiplexer".chars().count() - 1;
        let values = (0..=last_index)
            .map(|index| interpolate_ansi_value(54, 159, index, last_index))
            .collect::<Vec<_>>();
        assert_eq!(values.first().copied(), Some(54));
        assert_eq!(values.last().copied(), Some(159));
        assert_eq!(values.len(), "agent multiplexer".chars().count());
        assert!(values.windows(2).all(|window| window[0] <= window[1]));
        assert!(values.windows(7).all(|window| window.first() != window.last()));
    }

    #[test]
    fn host_picker_gradient_handles_short_strings() {
        assert_eq!(strip_ansi(&render_host_picker_gradient_text("")), "");
        let single = render_host_picker_gradient_text("a");
        assert_eq!(strip_ansi(&single), "a");
        assert_eq!(interpolate_ansi_value(54, 159, 0, 0), 54);
    }

    #[test]
    fn session_list_renders_title_header_and_rows_without_selector() {
        let session = demo("alpha", "repo-a");
        let rendered = render_session_list_lines(&[session], 120).join("\n");

        assert!(!rendered.contains("agentd - "));
        assert!(rendered.contains("STATUS"));
        assert!(rendered.contains("SESSION"));
        assert!(rendered.contains("TITLE"));
        assert!(rendered.contains("running"));
        assert!(!rendered.contains("› "));
    }

    #[test]
    fn session_list_uses_single_row_and_dynamic_widths() {
        let mut session = demo("alpha", "repository-with-a-long-name");
        session.title =
            "a very long title that should be truncated in the fixed width column".to_string();
        session.branch =
            "agent/this-is-a-very-long-branch-name-that-should-be-truncated".to_string();
        session.attention_summary =
            Some("needs manual review because a long follow-up summary is present".to_string());

        let wide = render_session_list_lines(&[session.clone()], 120).join("\n");
        let narrow = render_session_list_lines(&[session], 80).join("\n");

        assert_eq!(wide.lines().count(), 2);
        assert!(!wide.contains("needs manual review because a long follow-up summary is present"));
        assert!(wide.contains("a very long title that shou..."));
        assert!(wide.contains("agent/this-is-a-very-long-branc..."));
        assert!(narrow.contains("a very lo..."));
        assert!(narrow.contains("agent/this-is-a..."));
    }

    #[test]
    fn session_list_shows_empty_state() {
        let rendered = render_session_list_lines(&[], 120).join("\n");

        assert!(rendered.contains("No sessions."));
        assert!(rendered.contains("STATUS"));
    }

    #[test]
    fn session_list_does_not_render_legend() {
        let rendered = render_session_list_lines(
            &[
                demo_with("alpha", "repo-a", SessionStatus::NeedsInput, IntegrationState::Clean),
                demo_with(
                    "beta",
                    "repo-b",
                    SessionStatus::UnknownRecovered,
                    IntegrationState::Clean,
                ),
            ],
            120,
        )
        .join("\n");

        assert!(!rendered.contains(" • "));
        assert!(!rendered.contains("pending review"));
    }

    #[test]
    fn session_list_header_only_wraps_its_content() {
        let wide_layout = session_list_layout(200);
        let narrow_layout = session_list_layout(80);
        let wide = strip_ansi(&render_session_list_header_row(wide_layout));
        let narrow = strip_ansi(&render_session_list_header_row(narrow_layout));

        assert_eq!(wide, render_session_list_header_content(wide_layout));
        assert_eq!(wide.chars().count(), 120);
        assert_eq!(narrow, render_session_list_header_content(narrow_layout));
        assert_eq!(narrow.chars().count(), 80);
    }

    #[test]
    fn host_picker_legend_is_hidden_in_submenus() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::CreateAgentSelect { selected: 0 },
            detail_text: String::new(),
            detail_scroll: 0,
            toast: None,
        };

        let rendered = picker.render_lines(200, true).join("\n");
        assert!(!rendered.contains("needs input"));
        assert!(!rendered.contains("pending review"));
    }

    #[test]
    fn host_picker_session_row_uses_exit_time_for_finished_sessions() {
        let mut session =
            demo_with("alpha", "repo-a", SessionStatus::Exited, IntegrationState::Clean);
        let created_at = Utc::now() - Duration::hours(5);
        session.created_at = created_at;
        session.exited_at = Some(created_at + Duration::minutes(90));

        let rendered = render_host_picker_session_row(&session, 120, false);

        assert!(rendered.contains("1h  alpha"));
    }

    #[test]
    fn diff_view_renders_and_hides_legend() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha", "repo-a")],
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::DiffView { session_id: "alpha".to_string() },
            detail_text: "@@ -1 +1 @@\n-old\n+new\n".to_string(),
            detail_scroll: 0,
            toast: None,
        };

        let rendered = picker.render_lines(200, true).join("\n");
        assert!(rendered.contains("Diff: alpha"));
        assert!(rendered.contains(HOST_PICKER_DIFF_HUNK_STYLE));
        assert!(rendered.contains(HOST_PICKER_DIFF_REMOVE_STYLE));
        assert!(rendered.contains(HOST_PICKER_DIFF_ADD_STYLE));
        assert!(rendered.contains("@@ -1 +1 @@"));
        assert!(rendered.contains("-old"));
        assert!(rendered.contains("+new"));
        assert!(!rendered.contains("pending review"));
        assert!(!rendered.contains("needs input"));
    }

    #[test]
    fn style_host_picker_diff_line_colors_diff_sections() {
        assert!(
            style_host_picker_diff_line("diff --git a/a b/a", 80)
                .contains(HOST_PICKER_DIFF_HEADER_STYLE)
        );
        assert!(
            style_host_picker_diff_line("@@ -1 +1 @@", 80).contains(HOST_PICKER_DIFF_HUNK_STYLE)
        );
        assert!(style_host_picker_diff_line("+new", 80).contains(HOST_PICKER_DIFF_ADD_STYLE));
        assert!(style_host_picker_diff_line("-old", 80).contains(HOST_PICKER_DIFF_REMOVE_STYLE));
    }

    #[test]
    fn fit_host_picker_line_truncates_to_width() {
        assert_eq!(fit_host_picker_line("abcdef".to_string(), 4), "abc");
    }

    #[test]
    fn style_host_picker_query_line_adds_background_color() {
        let rendered = style_host_picker_query_line("", 40, true);
        assert!(rendered.starts_with(HOST_PICKER_QUERY_BG));
        assert!(rendered.ends_with(ANSI_RESET));
    }

    #[test]
    fn style_host_picker_query_line_uses_placeholder_and_cursor() {
        let rendered = style_host_picker_query_line("", 60, true);
        assert!(rendered.contains(HOST_PICKER_PLACEHOLDER_FG));
        assert!(rendered.contains("Type to filter sessions or name a new one."));
        assert!(rendered.contains(HOST_PICKER_CURSOR));
        assert!(rendered.contains("› "));
        let cursor_index = rendered.find(HOST_PICKER_CURSOR).unwrap();
        let placeholder_index =
            rendered.find("Type to filter sessions or name a new one.").unwrap();
        assert!(cursor_index < placeholder_index);
    }

    #[test]
    fn style_host_picker_query_line_hides_placeholder_when_typing() {
        let rendered = style_host_picker_query_line("abc", 30, false);
        assert!(rendered.contains(HOST_PICKER_TEXT_FG));
        assert!(rendered.contains("› "));
        assert!(rendered.contains("abc"));
        assert!(!rendered.contains("Type to filter sessions or name a new one."));
    }

    #[test]
    fn style_host_picker_background_row_uses_query_background() {
        let rendered = style_host_picker_background_row(20);
        assert!(rendered.starts_with(HOST_PICKER_QUERY_BG));
        assert!(rendered.ends_with(ANSI_RESET));
    }

    #[test]
    fn host_picker_sequences_do_not_enter_alternate_screen() {
        assert!(!HOST_PICKER_ENTER_SEQUENCE.windows(6).any(|window| window == b"\x1b[?1049"));
        assert!(!HOST_PICKER_EXIT_SEQUENCE.windows(6).any(|window| window == b"\x1b[?1049"));
    }

    fn demo(session_id: &str, repo_name: &str) -> SessionRecord {
        demo_with(session_id, repo_name, SessionStatus::Running, IntegrationState::Clean)
    }

    fn demo_with(
        session_id: &str,
        repo_name: &str,
        status: SessionStatus,
        integration_state: IntegrationState,
    ) -> SessionRecord {
        let (apply_state, merge_status) = match integration_state {
            IntegrationState::Clean => (ApplyState::Idle, MergeStatus::Unknown),
            IntegrationState::PendingReview => (ApplyState::Idle, MergeStatus::Ready),
            IntegrationState::Blocked => (ApplyState::Idle, MergeStatus::Blocked),
            IntegrationState::Conflicted => (ApplyState::Idle, MergeStatus::Conflicted),
        };
        let now = Utc::now();
        SessionRecord {
            session_id: session_id.to_string(),
            thread_id: Some(format!("thread-{session_id}")),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/repo".to_string(),
            repo_path: "/tmp/repo".to_string(),
            repo_name: repo_name.to_string(),
            title: format!("title-{session_id}"),
            base_branch: "main".to_string(),
            branch: format!("agent/{session_id}"),
            worktree: format!("/tmp/{session_id}"),
            status,
            integration_policy: IntegrationPolicy::AutoApplySafe,
            apply_state,
            merge_status,
            merge_summary: None,
            has_conflicts: false,
            pid: Some(123),
            exit_code: None,
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: None,
            created_at: now,
            updated_at: now,
            exited_at: None,
        }
    }

    fn test_paths() -> AppPaths {
        let root = Utf8PathBuf::from("/tmp/runtime-test");
        AppPaths {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            worktrees_dir: root.join("worktrees"),
            root,
        }
    }

    fn default_composer() -> PickerComposer {
        PickerComposer { query: String::new(), selected: 0 }
    }
}
