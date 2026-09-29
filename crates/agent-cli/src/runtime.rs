use std::{io::Write, time::Duration};

use anyhow::{Context, Result, bail};
use crossterm::{
    cursor::{MoveToColumn, MoveUp},
    event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers},
    execute,
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
    header::agentd_header,
    paths::AppPaths,
    protocol::{Request, Response},
    session::{SESSION_NAME_RULES, SessionRecord, SessionStatus, validate_session_name},
};

use crate::{
    CODEX_MODELS, RawModeGuard, StatusString, centered_rect, daemon_get_session,
    daemon_list_sessions, kill_session, send_request,
    session_display::{
        RunState, build_session_display_row, escape_controls, render_run_icon,
        session_elapsed_label, session_run_state, style_age, style_cwd, style_name, style_run,
    },
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Command {
    SessionSwitcher,
    NewSession,
    SessionDetails,
    StopSession,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum OverlayMode {
    Palette,
    SessionSwitcher,
    NewSession { edit_agent: bool },
    SessionDetails,
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
    SwitchSession(String),
}

const HOST_PICKER_QUERY_BG: &str = "\x1b[48;2;62;63;71m";
const HOST_PICKER_PLACEHOLDER_FG: &str = "\x1b[38;2;151;152;153m";
const HOST_PICKER_TEXT_FG: &str = "\x1b[38;2;255;255;255m";
const HOST_PICKER_STATUS_RED_FG: &str = "\x1b[31m";
const HOST_PICKER_STATUS_GREEN_FG: &str = "\x1b[32m";
const HOST_PICKER_SELECTED_STYLE: &str = "\x1b[34m";
const HOST_PICKER_LEGEND_TEXT_STYLE: &str = "\x1b[90m";
const HOST_PICKER_CURSOR: &str = "█";
const HOST_PICKER_CURSOR_BLINK_MS: u128 = 500;
const ANSI_RESET: &str = "\x1b[0m";
const HOST_PICKER_ENTER_SEQUENCE: &[u8] = b"\x1b[?25l";
const HOST_PICKER_EXIT_SEQUENCE: &[u8] = b"\x1b[?25h";
const SESSION_LIST_DEFAULT_WIDTH: usize = 120;
const SESSION_LIST_RUN_WIDTH: usize = 3;
const SESSION_LIST_AGE_WIDTH: usize = 5;
const SESSION_LIST_NAME_MIN_WIDTH: usize = 10;
const SESSION_LIST_NAME_MAX_WIDTH: usize = 26;
const SESSION_LIST_NAME_FLOOR_WIDTH: usize = 8;
const SESSION_LIST_CWD_MIN_WIDTH: usize = 16;
const SESSION_LIST_CWD_MAX_WIDTH: usize = 48;
const SESSION_LIST_CWD_FLOOR_WIDTH: usize = 10;
const SESSION_LIST_STRUCTURAL_WIDTH: usize = 8;
const HOST_PICKER_SESSION_PREFIX_WIDTH: usize = 4;
const HOST_PICKER_SESSION_STRUCTURAL_WIDTH: usize = 2;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct SessionListLayout {
    visible_width: usize,
    run: usize,
    age: usize,
    name: usize,
    cwd: usize,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct HostPickerSessionLayout {
    age: usize,
    name: usize,
    cwd: usize,
}

struct PickerScreenGuard;

impl PickerScreenGuard {
    fn enter() -> Result<Self> {
        write_screen_bytes(HOST_PICKER_ENTER_SEQUENCE)
            .context("failed to prepare session picker")?;
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
        names.push("claude".to_string());
    }
    Ok(names)
}

fn configured_default_agent(paths: &AppPaths) -> Result<String> {
    let config = Config::load(paths)?;
    Ok(config.default_agent_name(paths)?.to_string())
}

fn ordered_create_agents(mut names: Vec<String>, default_agent: Option<&str>) -> Vec<String> {
    let Some(default_agent) = default_agent else {
        return names;
    };
    let Some(index) = names.iter().position(|agent| agent == default_agent) else {
        return names;
    };
    if index > 0 {
        let default = names.remove(index);
        names.insert(0, default);
    }
    names
}

fn create_agent_label(agent: &str, default_agent: Option<&str>) -> String {
    if default_agent == Some(agent) { format!("{agent} (Default)") } else { agent.to_string() }
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
    if previous_lines == 0 {
        write!(stdout, "\r\n").context("failed to start session picker below prompt")?;
    }
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
    execute!(
        stdout,
        MoveToColumn(0),
        MoveUp(previous_lines.saturating_sub(1) as u16),
        Clear(ClearType::FromCursorDown)
    )
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

    for session in ordered_sessions(sessions) {
        lines.push(render_session_list_row(session, layout));
    }

    lines
}

struct SessionPicker {
    paths: AppPaths,
    sessions: Vec<SessionRecord>,
    default_agent: Option<String>,
    create_agents: Vec<String>,
    composer: PickerComposer,
    mode: PickerMode,
    toast: Option<PickerToast>,
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
    DeleteConfirm { session_id: String, selected: usize },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SessionAction {
    Attach,
    Delete,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ConfirmAction {
    Yes,
    No,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PickerToast {
    kind: PickerToastKind,
    message: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PickerToastKind {
    Notice,
    Error,
}

#[derive(Clone, Debug)]
struct PickerComposer {
    query: String,
    selected: usize,
}

impl SessionPicker {
    fn new(paths: AppPaths) -> Self {
        let default_agent = configured_default_agent(&paths).ok();
        let create_agents = ordered_create_agents(
            configured_agent_names(&paths).unwrap_or_else(|_| vec!["claude".to_string()]),
            default_agent.as_deref(),
        );
        Self {
            paths,
            sessions: Vec::new(),
            default_agent,
            create_agents,
            composer: PickerComposer { query: String::new(), selected: 0 },
            mode: PickerMode::Browse,
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
        self.composer.selected = 0;
    }

    async fn handle_composer_key(&mut self, key: KeyEvent) -> Result<Option<Option<String>>> {
        let row_count = self.picker_rows().len();
        match key.code {
            KeyCode::Esc => return Ok(Some(None)),
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
                    if !self.composer.query.trim().is_empty()
                        && self.composer_requested_name().is_none()
                    {
                        self.toast = Some(PickerToast::error(format!(
                            "invalid session name: {SESSION_NAME_RULES}"
                        )));
                        return Ok(None);
                    }
                    self.open_create_agent_menu();
                }
            },
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                self.toast = None;
                self.composer.query.push(ch);
                self.clamp_selection();
                self.composer.selected = 0;
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
                    self.toast = Some(PickerToast::notice(format!("removed session {session_id}")));
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
                self.toast = Some(PickerToast::notice(format!("removed session {session_id}")));
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
                        lines.push(render_host_picker_create_row(&label, width, selected));
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
            lines.push(render_host_picker_toast_line(toast, width));
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
        let heading = self
            .session_by_id(session_id)
            .map(|session| format!("{}  {}", session.session_id, session.agent))
            .unwrap_or_else(|| session_id.to_string());
        lines.push(style_host_picker_menu_line(
            &fit_host_picker_line(heading, width),
            width,
            false,
        ));
        if let Some(session) = self.session_by_id(session_id) {
            lines.push(style_host_picker_menu_line(
                &fit_host_picker_line(escape_controls(&session.cwd), width),
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
        let heading = if self.composer.query.trim().is_empty() {
            "Choose coding agent".to_string()
        } else if self.composer_requested_name().is_some() {
            format!("Create: {}", self.composer.query.trim())
        } else {
            "Invalid session name".to_string()
        };
        lines.push(style_host_picker_menu_line(
            &fit_host_picker_line(heading, width),
            width,
            false,
        ));
        for (index, agent) in self.create_agents.iter().enumerate() {
            let label = format!(
                "{}. {}",
                index + 1,
                create_agent_label(agent, self.default_agent.as_deref())
            );
            lines.push(style_host_picker_menu_line(&label, width, index == selected));
        }
        lines.push(style_host_picker_background_row(width));
        let help =
            if self.composer.query.trim().is_empty() || self.composer_requested_name().is_some() {
                "Enter selects. Esc goes back.".to_string()
            } else {
                format!("Use lowercase letters, numbers, and hyphens. {}", SESSION_NAME_RULES)
            };
        lines.push(fit_host_picker_line(help, width));
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
                &fit_host_picker_line(escape_controls(&session.cwd), width),
                width,
                false,
            ));
        }
        lines.push(style_host_picker_menu_line(
            &format!("Delete {session_id}? Its working directory is kept."),
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

    fn filtered_sessions(&self) -> Vec<&SessionRecord> {
        ordered_sessions(&self.sessions)
            .into_iter()
            .filter(|session| matches_query(session_search_text(session), &self.composer.query))
            .collect()
    }

    fn picker_rows(&self) -> Vec<PickerRow> {
        let mut rows = self
            .filtered_sessions()
            .into_iter()
            .map(|session| PickerRow::Session(session.session_id.clone()))
            .collect::<Vec<_>>();
        rows.push(PickerRow::Create);
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

    fn composer_requested_name(&self) -> Option<String> {
        let trimmed = self.composer.query.trim();
        if trimmed.is_empty() {
            return None;
        }
        validate_session_name(trimmed).ok()?;
        Some(trimmed.to_string())
    }

    fn refresh_agent_names(&mut self) -> Result<()> {
        self.default_agent = configured_default_agent(&self.paths).ok();
        self.create_agents = ordered_create_agents(
            configured_agent_names(&self.paths)?,
            self.default_agent.as_deref(),
        );
        if self.create_agents.is_empty() {
            self.toast = Some(PickerToast::notice(
                "no configured agents found; falling back to claude".to_string(),
            ));
            self.create_agents = vec!["claude".to_string()];
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
        if session_accepts_attach(session) {
            actions.push(SessionAction::Attach);
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
        self.default_agent
            .as_deref()
            .and_then(|default_agent| {
                self.create_agents.iter().position(|agent| agent == default_agent)
            })
            .unwrap_or(0)
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
            SessionAction::Delete => {
                self.open_delete_confirmation(&session_id, 1);
                Ok(None)
            }
        }
    }

    async fn create_session_with_agent(&mut self, agent: &str) -> Result<Option<Option<String>>> {
        let name = self.composer_requested_name();
        if !self.composer.query.trim().is_empty() && name.is_none() {
            self.toast =
                Some(PickerToast::error(format!("invalid session name: {SESSION_NAME_RULES}")));
            return Ok(None);
        }
        if let Some(host) = crate::transport::remote_name() {
            self.toast = Some(PickerToast::error(remote_create_hint(host)));
            return Ok(None);
        }
        let cwd = crate::resolve_cwd(None)?;
        let response = send_request(
            &self.paths,
            &Request::CreateSession {
                cwd,
                name,
                agent: agent.to_string(),
                model: if agent == "codex" { Some(CODEX_MODELS[0].to_string()) } else { None },
                workspace: None,
            },
        )
        .await?;
        match response {
            Response::CreateSession { session } => Ok(Some(Some(session.session_id))),
            Response::Error { message } => {
                self.toast = Some(PickerToast::error(message));
                Ok(None)
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
}

impl PickerToast {
    fn notice(message: String) -> Self {
        Self { kind: PickerToastKind::Notice, message }
    }

    fn error(message: String) -> Self {
        Self { kind: PickerToastKind::Error, message }
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

fn sanitize_picker_message(message: &str) -> String {
    message.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn render_host_picker_toast_line(toast: &PickerToast, width: usize) -> String {
    let prefix = match toast.kind {
        PickerToastKind::Notice => "notice",
        PickerToastKind::Error => "error",
    };
    let line = fit_host_picker_line(
        format!("{prefix}: {}", sanitize_picker_message(&toast.message)),
        width,
    );
    match toast.kind {
        PickerToastKind::Notice => line,
        PickerToastKind::Error => format!("{HOST_PICKER_STATUS_RED_FG}{line}{ANSI_RESET}"),
    }
}

fn render_host_picker_title_line(_width: usize) -> String {
    agentd_header()
}

fn render_host_picker_create_row(label: &str, width: usize, selected: bool) -> String {
    let max_chars = width.saturating_sub(1).max(1);
    let leader = if selected { "› ".to_string() } else { "  ".to_string() };
    let prefix = format!("{leader}+ ");
    let remaining = max_chars.saturating_sub(prefix.chars().count());
    let visible_tail = truncate_session_list_cell(label, remaining);
    let tail_style = if selected { HOST_PICKER_SELECTED_STYLE } else { "" };
    format!("{tail_style}{prefix}{visible_tail}{ANSI_RESET}")
}

fn render_session_list_header_row(layout: SessionListLayout) -> String {
    style_host_picker_content_row(&render_session_list_header_content(layout), layout.visible_width)
}

fn render_session_list_row(session: &SessionRecord, layout: SessionListLayout) -> String {
    let row = build_session_display_row(session);
    let run = render_session_list_run_cell(&row, layout.run);
    let age = style_age(&format_session_list_cell(&row.age_text, layout.age));
    let name = style_name(&format_session_list_cell(&row.name, layout.name));
    let cwd = style_cwd(&format_path_cell(&row.cwd, layout.cwd));
    format!("  {run}  {age}  {name}  {cwd}")
}

fn render_session_list_header_content(layout: SessionListLayout) -> String {
    format!(
        "  {}  {}  {}  {}",
        format_session_list_cell("RUN", layout.run),
        format_session_list_cell("AGE", layout.age),
        format_session_list_cell("NAME", layout.name),
        format_session_list_cell("CWD", layout.cwd),
    )
}

fn render_session_list_run_cell(
    row: &crate::session_display::SessionDisplayRow,
    width: usize,
) -> String {
    style_run(&format_session_list_cell(render_run_icon(row.run_state), width), row.run_state)
}

fn style_host_picker_content_row(content: &str, visible_width: usize) -> String {
    let visible = content.chars().take(visible_width).collect::<String>();
    format!("{HOST_PICKER_QUERY_BG}{HOST_PICKER_TEXT_FG}{visible}{ANSI_RESET}")
}

fn session_list_layout(width: usize) -> SessionListLayout {
    let visible_width = session_list_visible_width(width);
    let mut layout = SessionListLayout {
        visible_width,
        run: SESSION_LIST_RUN_WIDTH,
        age: SESSION_LIST_AGE_WIDTH,
        name: SESSION_LIST_NAME_MIN_WIDTH,
        cwd: SESSION_LIST_CWD_MIN_WIDTH,
    };
    let min_total = session_list_total_width(layout);
    if visible_width >= min_total {
        let mut remaining = visible_width - min_total;
        grow_session_list_column(&mut layout.cwd, SESSION_LIST_CWD_MAX_WIDTH, &mut remaining);
        grow_session_list_column(&mut layout.name, SESSION_LIST_NAME_MAX_WIDTH, &mut remaining);
        return layout;
    }

    let mut deficit = min_total - visible_width;
    shrink_session_list_column(&mut layout.name, SESSION_LIST_NAME_FLOOR_WIDTH, &mut deficit);
    shrink_session_list_column(&mut layout.cwd, SESSION_LIST_CWD_FLOOR_WIDTH, &mut deficit);
    layout
}

fn session_list_visible_width(width: usize) -> usize {
    width.max(1).min(SESSION_LIST_DEFAULT_WIDTH)
}

fn session_list_total_width(layout: SessionListLayout) -> usize {
    SESSION_LIST_STRUCTURAL_WIDTH + layout.run + layout.age + layout.name + layout.cwd
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

fn host_picker_session_layout(width: usize) -> HostPickerSessionLayout {
    let body_width =
        width.saturating_sub(1).max(1).saturating_sub(HOST_PICKER_SESSION_PREFIX_WIDTH);
    let mut layout = HostPickerSessionLayout {
        age: SESSION_LIST_AGE_WIDTH,
        name: SESSION_LIST_NAME_MIN_WIDTH,
        cwd: SESSION_LIST_CWD_MIN_WIDTH,
    };
    let min_total = host_picker_session_total_width(layout);
    if body_width >= min_total {
        let mut remaining = body_width - min_total;
        grow_session_list_column(&mut layout.cwd, SESSION_LIST_CWD_MAX_WIDTH, &mut remaining);
        grow_session_list_column(&mut layout.name, SESSION_LIST_NAME_MAX_WIDTH, &mut remaining);
        return layout;
    }

    let mut deficit = min_total - body_width;
    shrink_session_list_column(&mut layout.name, SESSION_LIST_NAME_FLOOR_WIDTH, &mut deficit);
    shrink_session_list_column(&mut layout.cwd, SESSION_LIST_CWD_FLOOR_WIDTH, &mut deficit);
    layout
}

fn host_picker_session_total_width(layout: HostPickerSessionLayout) -> usize {
    HOST_PICKER_SESSION_STRUCTURAL_WIDTH + layout.age + layout.name + layout.cwd
}

fn format_session_list_cell(content: &str, width: usize) -> String {
    let truncated = truncate_session_list_cell(content, width);
    format!("{truncated:<width$}")
}

/// Like `format_session_list_cell`, but truncates from the left so the end of
/// a path (usually the most specific part) stays visible.
fn format_path_cell(content: &str, width: usize) -> String {
    let char_count = content.chars().count();
    let truncated = if char_count <= width {
        content.to_string()
    } else if width <= 3 {
        content.chars().skip(char_count - width).collect()
    } else {
        let tail = content.chars().skip(char_count - (width - 3)).collect::<String>();
        format!("...{tail}")
    };
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
    name_input: String,
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
            name_input: String::new(),
            // Filled with the configured default when the new-session form opens.
            agent_input: String::new(),
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

    pub fn draw(&mut self) -> Result<()> {
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
            && !matches!(self.mode, OverlayMode::SessionDetails | OverlayMode::Palette)
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
            OverlayMode::SessionDetails => self.handle_detail_key(key).await,
            OverlayMode::StopConfirm => self.handle_stop_confirm_key(key).await,
        }
    }

    fn handle_paste(&mut self, data: &str) {
        if let OverlayMode::NewSession { edit_agent } = self.mode {
            if edit_agent {
                self.agent_input.push_str(data);
            } else {
                self.name_input.push_str(data);
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
                    self.name_input.pop();
                }
            }
            KeyCode::Enter => {
                let name = self.name_input.trim();
                let agent = self.agent_input.trim();
                if agent.is_empty() {
                    self.toast = Some("agent cannot be empty".to_string());
                    return Ok(None);
                }
                if !name.is_empty() && validate_session_name(name).is_err() {
                    self.toast = Some(format!("invalid session name: {SESSION_NAME_RULES}"));
                    return Ok(None);
                }
                if let Some(host) = crate::transport::remote_name() {
                    self.toast = Some(remote_create_hint(host));
                    return Ok(None);
                }
                let cwd = crate::resolve_cwd(None)?;
                let response = send_request(
                    &self.paths,
                    &Request::CreateSession {
                        cwd,
                        name: (!name.is_empty()).then(|| name.to_string()),
                        agent: agent.to_string(),
                        model: if agent == "codex" {
                            Some(CODEX_MODELS[0].to_string())
                        } else {
                            None
                        },
                        workspace: None,
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
                    self.name_input.push(ch);
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
                self.name_input.clear();
                self.agent_input =
                    configured_default_agent(&self.paths).unwrap_or_else(|_| "claude".to_string());
            }
            Command::SessionDetails => {
                let session = daemon_get_session(&self.paths, &self.session_id).await?;
                self.detail_text = format!(
                    "name       {}\nagent      {}\ncwd        {}\nstatus     {}",
                    session.session_id,
                    session.agent,
                    session.cwd,
                    session_status_text(&session),
                );
                self.detail_scroll = 0;
                self.mode = OverlayMode::SessionDetails;
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
            OverlayMode::SessionDetails => "Session Details",
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
            OverlayMode::SessionDetails => self.render_detail(frame, inner),
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
                let name_style = if index == self.switcher_selected {
                    Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
                } else {
                    Style::default().add_modifier(Modifier::BOLD)
                };
                let row = build_session_display_row(session);
                Line::from(vec![
                    Span::styled(render_run_icon(row.run_state), run_state_style(row.run_state)),
                    Span::raw("  "),
                    Span::styled(session_elapsed_label(session), subtle_style()),
                    Span::raw("  "),
                    Span::styled(session.session_id.as_str(), name_style),
                    Span::raw("  "),
                    Span::styled(row.cwd, subtle_style()),
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
        let name_style =
            if edit_agent { Style::default() } else { Style::default().fg(Color::Cyan) };
        let agent_style =
            if edit_agent { Style::default().fg(Color::Cyan) } else { Style::default() };
        frame.render_widget(
            Paragraph::new(Line::from(vec![
                Span::styled("name   ", subtle_style()),
                Span::styled(self.name_input.as_str(), name_style),
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
            Paragraph::new("Blank auto-generates. Use lowercase letters, numbers, and hyphens."),
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
        ordered_sessions(&self.sessions)
            .into_iter()
            .filter(|session| matches_query(session_search_text(session), &self.switcher_query))
            .collect()
    }
}

fn filtered_palette_items(query: &str) -> Vec<PaletteItem> {
    palette_items()
        .into_iter()
        .filter(|item| matches_query(format!("{} {}", item.key_hint, item.title), query))
        .collect()
}

pub(crate) fn compare_session_switcher_order(
    left: &SessionRecord,
    right: &SessionRecord,
) -> std::cmp::Ordering {
    session_sort_bucket(left)
        .cmp(&session_sort_bucket(right))
        .then_with(|| right.updated_at.cmp(&left.updated_at))
}

pub(crate) fn ordered_sessions(sessions: &[SessionRecord]) -> Vec<&SessionRecord> {
    let mut sessions = sessions.iter().collect::<Vec<_>>();
    sessions.sort_by(|left, right| compare_session_switcher_order(left, right));
    sessions
}

pub(crate) fn session_accepts_attach(session: &SessionRecord) -> bool {
    session.status == SessionStatus::Running
}

fn palette_items() -> Vec<PaletteItem> {
    vec![
        PaletteItem { key_hint: "s", title: "Switch Session", command: Command::SessionSwitcher },
        PaletteItem { key_hint: "t", title: "New Session", command: Command::NewSession },
        PaletteItem { key_hint: "i", title: "Session Details", command: Command::SessionDetails },
        PaletteItem { key_hint: "x", title: "Stop Session", command: Command::StopSession },
    ]
}

fn session_search_text(session: &SessionRecord) -> String {
    format!(
        "{} {} {} {} {}",
        session.session_id,
        session.agent,
        session.cwd,
        session.status_string(),
        session.attention_string(),
    )
}

fn session_sort_bucket(session: &SessionRecord) -> u8 {
    if matches!(session.status, SessionStatus::Creating | SessionStatus::Running) {
        return 0;
    }

    let row = build_session_display_row(session);
    match row.run_state {
        RunState::Failed | RunState::Recovered => 2,
        _ if row.needs_attention => 3,
        RunState::Exited => 4,
        RunState::Running | RunState::Starting => unreachable!(),
    }
}

/// A new session's directory is on the remote machine, which the picker has
/// no way to browse, so it points at the command that takes one.
fn remote_create_hint(host: &str) -> String {
    format!("start sessions on `{host}` with `agent --host {host} new --cwd DIR`")
}

fn session_icon(session: &SessionRecord) -> &'static str {
    render_run_icon(session_run_state(session))
}

fn session_icon_color(session: &SessionRecord) -> Color {
    match session_run_state(session) {
        RunState::Starting | RunState::Running => Color::Green,
        RunState::Exited | RunState::Recovered => Color::DarkGray,
        RunState::Failed => Color::Red,
    }
}

fn render_host_picker_session_row(session: &SessionRecord, width: usize, selected: bool) -> String {
    let row = build_session_display_row(session);
    let leader = if selected { "› ".to_string() } else { "  ".to_string() };
    let layout = host_picker_session_layout(width);
    let tail_style = if selected { HOST_PICKER_SELECTED_STYLE } else { "" };
    let run = style_run(render_run_icon(row.run_state), row.run_state);
    let age = style_age(&format_session_list_cell(&row.age_text, layout.age));
    let name = style_name(&format_session_list_cell(&row.name, layout.name));
    let cwd = style_cwd(&format_path_cell(&row.cwd, layout.cwd));
    format!("{tail_style}{leader}{ANSI_RESET}{run} {tail_style}{age} {name} {cwd}{ANSI_RESET}",)
}

fn render_host_picker_legend_row(width: usize, sessions: &[SessionRecord]) -> String {
    let entries = [
        (RunState::Running, "running"),
        (RunState::Exited, "exited"),
        (RunState::Recovered, "lost"),
        (RunState::Failed, "failed"),
    ];
    let max_chars = width.saturating_sub(1).max(1);
    let mut line = "  ".to_string();
    let matching_entries = entries
        .into_iter()
        .filter(|(run_state, _)| {
            sessions.iter().any(|session| session_run_state(session) == *run_state)
        })
        .map(|(run_state, label)| {
            (style_run(render_run_icon(run_state), run_state), label.to_string())
        })
        .collect::<Vec<_>>();
    if matching_entries.is_empty() {
        return String::new();
    }
    let mut visible = 0usize;
    for (index, (styled_icon, plain_entry)) in matching_entries.into_iter().enumerate() {
        let separator = if index > 0 {
            format!("{HOST_PICKER_LEGEND_TEXT_STYLE} • {ANSI_RESET}")
        } else {
            String::new()
        };
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
            "{styled_icon}{HOST_PICKER_LEGEND_TEXT_STYLE} {plain_entry}{ANSI_RESET}"
        ));
        visible += plain_entry.chars().count();
    }
    line
}

fn session_status_text(session: &SessionRecord) -> String {
    if let Some(summary) = &session.attention_summary {
        return summary.clone();
    }
    match session_run_state(session) {
        RunState::Starting => "starting".to_string(),
        RunState::Running => "running".to_string(),
        RunState::Failed => session.error.clone().unwrap_or_else(|| "failed".to_string()),
        RunState::Recovered => "daemon lost the live process".to_string(),
        RunState::Exited => match session.exit_code {
            Some(code) => format!("exited ({code})"),
            None => "exited".to_string(),
        },
    }
}

fn run_state_style(run_state: RunState) -> Style {
    let color = match run_state {
        RunState::Starting | RunState::Running => Color::Green,
        RunState::Exited | RunState::Recovered => Color::DarkGray,
        RunState::Failed => Color::Red,
    };
    Style::default().fg(color)
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
        ANSI_RESET, AttachOverlay, HOST_PICKER_CURSOR, HOST_PICKER_ENTER_SEQUENCE,
        HOST_PICKER_EXIT_SEQUENCE, HOST_PICKER_LEGEND_TEXT_STYLE, HOST_PICKER_PLACEHOLDER_FG,
        HOST_PICKER_QUERY_BG, HOST_PICKER_SELECTED_STYLE, HOST_PICKER_STATUS_GREEN_FG,
        HOST_PICKER_STATUS_RED_FG, HOST_PICKER_TEXT_FG, OverlayMode, OverlayOutcome,
        PickerComposer, PickerMode, PickerRow, PickerToast, SessionAction, SessionPicker,
        configured_agent_names, fit_host_picker_line, format_path_cell, ordered_create_agents,
        ordered_sessions, render_host_picker_create_row, render_host_picker_legend_row,
        render_host_picker_session_row, render_host_picker_title_line,
        render_host_picker_toast_line, render_session_list_header_content,
        render_session_list_header_row, render_session_list_lines, sanitize_picker_message,
        session_icon, session_icon_color, session_list_layout, style_host_picker_background_row,
        style_host_picker_query_line,
    };
    use agentd_shared::{
        paths::AppPaths,
        session::{AttentionLevel, SessionMode, SessionRecord, SessionStatus},
    };
    use camino::Utf8PathBuf;
    use chrono::{Duration, Utc};
    use crossterm::event::{Event, KeyCode, KeyEvent, KeyEventKind, KeyEventState, KeyModifiers};
    use futures::executor::block_on;

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
        picker.sessions = vec![demo("alpha"), demo("beta")];

        let rows = picker.picker_rows();
        assert_eq!(rows[2], PickerRow::Create);
        assert!(rows[..2].contains(&PickerRow::Session("alpha".to_string())));
        assert!(rows[..2].contains(&PickerRow::Session("beta".to_string())));
        assert_eq!(rows.len(), 3);
    }

    #[test]
    fn query_filter_keeps_create_row() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha"), demo("beta")];
        picker.composer = PickerComposer { query: "beta".to_string(), selected: 0 };

        let rows = picker.picker_rows();
        assert_eq!(rows, vec![PickerRow::Session("beta".to_string()), PickerRow::Create]);
    }

    #[test]
    fn clamp_selection_caps_to_available_rows() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha")];
        picker.composer.selected = 8;

        picker.clamp_selection();

        assert_eq!(picker.composer.selected, 1);
    }

    #[test]
    fn composer_q_is_treated_as_query_input() {
        let mut picker = SessionPicker::new(test_paths());

        let outcome = block_on(
            picker.handle_composer_key(KeyEvent::new(KeyCode::Char('q'), KeyModifiers::NONE)),
        )
        .unwrap();

        assert_eq!(outcome, None);
        assert_eq!(picker.composer.query, "q");
    }

    #[test]
    fn composer_input_selects_first_matching_session_before_create_row() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha"), demo("beta")];

        let outcome = block_on(
            picker.handle_composer_key(KeyEvent::new(KeyCode::Char('b'), KeyModifiers::NONE)),
        )
        .unwrap();

        assert_eq!(outcome, None);
        assert_eq!(picker.composer.selected, 0);
        assert_eq!(
            picker.picker_rows(),
            vec![PickerRow::Session("beta".to_string()), PickerRow::Create]
        );
    }

    #[test]
    fn paste_selects_first_matching_session_before_create_row() {
        let mut picker = SessionPicker::new(test_paths());
        picker.sessions = vec![demo("alpha"), demo("beta")];

        picker.handle_paste("beta");

        assert_eq!(picker.composer.selected, 0);
        assert_eq!(
            picker.picker_rows(),
            vec![PickerRow::Session("beta".to_string()), PickerRow::Create]
        );
    }

    #[test]
    fn composer_r_is_treated_as_query_input() {
        let mut picker = SessionPicker::new(test_paths());

        let outcome = block_on(
            picker.handle_composer_key(KeyEvent::new(KeyCode::Char('r'), KeyModifiers::NONE)),
        )
        .unwrap();

        assert_eq!(outcome, None);
        assert_eq!(picker.composer.query, "r");
    }

    #[test]
    fn composer_escape_still_closes_picker() {
        let mut picker = SessionPicker::new(test_paths());

        let outcome =
            block_on(picker.handle_composer_key(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE)))
                .unwrap();

        assert_eq!(outcome, Some(None));
    }

    #[test]
    fn attach_overlay_ctrl_y_is_ignored_in_palette() {
        let mut overlay = AttachOverlay::new(test_paths(), "alpha".to_string());
        overlay.mode = OverlayMode::Palette;

        let outcome =
            block_on(overlay.handle_key(KeyEvent::new(KeyCode::Char('y'), KeyModifiers::CONTROL)))
                .unwrap();

        assert_eq!(outcome, None);
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
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            toast: None,
        };

        assert_eq!(
            picker.action_items("alpha"),
            vec![SessionAction::Attach, SessionAction::Delete]
        );
    }

    #[test]
    fn action_menu_hides_attach_for_exited_sessions() {
        let mut session = demo("alpha");
        session.status = SessionStatus::Exited;
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![session],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            toast: None,
        };

        assert_eq!(picker.action_items("alpha"), vec![SessionAction::Delete]);
    }

    #[test]
    fn clamp_mode_selection_caps_to_visible_actions() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::SessionActions { session_id: "alpha".to_string(), selected: 9 },
            toast: None,
        };

        picker.clamp_mode_selection();

        assert_eq!(
            picker.mode,
            PickerMode::SessionActions { session_id: "alpha".to_string(), selected: 1 }
        );
    }

    #[test]
    fn clamp_mode_selection_resets_when_session_disappears() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: Vec::new(),
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::DeleteConfirm { session_id: "missing".to_string(), selected: 1 },
            toast: None,
        };

        picker.clamp_mode_selection();

        assert_eq!(picker.mode, PickerMode::Browse);
    }

    #[test]
    fn render_lines_hides_session_list_in_action_mode() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha"), demo("beta")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::SessionActions { session_id: "alpha".to_string(), selected: 0 },
            toast: None,
        };

        let lines = picker.render_lines(120, true);
        let rendered = lines.join("\n");

        assert!(!rendered.contains("/tmp/beta"));
        assert!(rendered.contains("› 1. attach"));
        assert!(rendered.contains("  2. delete"));
        assert!(rendered.contains("/tmp/alpha"));
    }

    #[test]
    fn render_lines_show_selector_on_delete_confirmation_rows() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::DeleteConfirm { session_id: "alpha".to_string(), selected: 1 },
            toast: None,
        };

        let lines = picker.render_lines(120, true);
        let rendered = lines.join("\n");

        assert!(rendered.contains("  1. yes"));
        assert!(rendered.contains("› 2. no"));
        assert!(rendered.contains("Its working directory is kept."));
    }

    #[test]
    fn render_lines_remove_helper_and_agent_row_in_browse_mode() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
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
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["codex".to_string(), "claude".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            toast: None,
        };

        picker.open_create_agent_menu();

        assert_eq!(picker.mode, PickerMode::CreateAgentSelect { selected: 0 });
    }

    #[test]
    fn create_agent_menu_clamps_and_renders_agents() {
        let mut picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["codex".to_string(), "claude".to_string()],
            composer: default_composer(),
            mode: PickerMode::CreateAgentSelect { selected: 9 },
            toast: None,
        };

        picker.clamp_mode_selection();
        let rendered = picker.render_lines(120, true).join("\n");

        assert_eq!(picker.mode, PickerMode::CreateAgentSelect { selected: 1 });
        assert!(rendered.contains("1. codex (Default)"));
        assert!(rendered.contains("› 2. claude"));
        assert!(!rendered.contains("/tmp/alpha"));
    }

    #[test]
    fn configured_agent_names_use_default_config_order() {
        let names = configured_agent_names(&test_paths()).unwrap();
        assert_eq!(names, vec!["claude".to_string(), "codex".to_string()]);
    }

    #[test]
    fn ordered_create_agents_moves_default_to_front_and_preserves_other_order() {
        let names = ordered_create_agents(
            vec!["codex".to_string(), "claude".to_string(), "zed".to_string()],
            Some("claude"),
        );

        assert_eq!(names, vec!["claude".to_string(), "codex".to_string(), "zed".to_string()]);
    }

    #[test]
    fn ordered_create_agents_keeps_order_when_default_is_missing() {
        let names = ordered_create_agents(
            vec!["codex".to_string(), "claude".to_string(), "zed".to_string()],
            None,
        );

        assert_eq!(names, vec!["codex".to_string(), "claude".to_string(), "zed".to_string()]);
    }

    #[test]
    fn runtime_icons_match_requested_symbols() {
        assert_eq!(session_icon(&demo_with("alpha", SessionStatus::Running)), "●");
        assert_eq!(session_icon(&demo_with("alpha", SessionStatus::Creating)), "●");
        assert_eq!(session_icon(&demo_with("alpha", SessionStatus::Failed)), "✖");
        assert_eq!(session_icon(&demo_with("alpha", SessionStatus::UnknownRecovered)), "○");
        assert_eq!(session_icon(&demo_with("alpha", SessionStatus::Exited)), "○");
    }

    #[test]
    fn runtime_icon_colors_match_requested_palette() {
        use ratatui::style::Color;
        assert_eq!(session_icon_color(&demo_with("alpha", SessionStatus::Running)), Color::Green);
        assert_eq!(session_icon_color(&demo_with("alpha", SessionStatus::Failed)), Color::Red);
        assert_eq!(
            session_icon_color(&demo_with("alpha", SessionStatus::UnknownRecovered)),
            Color::DarkGray
        );
        assert_eq!(session_icon_color(&demo_with("alpha", SessionStatus::Exited)), Color::DarkGray);
    }

    #[test]
    fn host_picker_session_row_includes_status_color_escape() {
        let mut running_session = demo_with("alpha", SessionStatus::Running);
        running_session.created_at = Utc::now() - Duration::minutes(23);
        let running = render_host_picker_session_row(&running_session, 120, true);
        let failed =
            render_host_picker_session_row(&demo_with("alpha", SessionStatus::Failed), 120, false);

        assert!(running.contains(HOST_PICKER_STATUS_GREEN_FG));
        assert!(failed.contains(HOST_PICKER_STATUS_RED_FG));
        assert!(running.contains(HOST_PICKER_SELECTED_STYLE));
        assert!(running.contains("› "));
        assert!(running.contains("●"));
        let running_plain = strip_ansi(&running);
        assert!(running_plain.contains("23m"));
        assert!(running_plain.contains("alpha"));
        assert!(running_plain.contains("/tmp/alpha"));
        assert!(failed.contains("✖"));
    }

    #[test]
    fn host_picker_legend_uses_colored_icons_and_separator() {
        let rendered = render_host_picker_legend_row(
            200,
            &[
                demo_with("alpha", SessionStatus::Running),
                demo_with("delta", SessionStatus::Exited),
                demo_with("gamma", SessionStatus::Failed),
            ],
        );
        assert!(rendered.contains(HOST_PICKER_STATUS_RED_FG));
        assert!(rendered.contains(HOST_PICKER_STATUS_GREEN_FG));
        assert!(rendered.contains(HOST_PICKER_LEGEND_TEXT_STYLE));
        assert!(rendered.starts_with("  "));
        assert!(rendered.contains("●"));
        assert!(rendered.contains("○"));
        assert!(rendered.contains("✖"));
        assert!(rendered.contains("running"));
        assert!(rendered.contains("exited"));
        assert!(rendered.contains("failed"));
        assert!(!rendered.contains("lost"));
        assert!(rendered.contains(" • "));
    }

    #[test]
    fn host_picker_title_uses_shared_header() {
        let title = render_host_picker_title_line(120);
        assert_eq!(strip_ansi(&title), "agentd - agent multiplexer");
    }

    #[test]
    fn host_picker_title_does_not_truncate_for_narrow_widths() {
        let title = render_host_picker_title_line(4);
        assert_eq!(strip_ansi(&title), "agentd - agent multiplexer");
    }

    #[test]
    fn session_list_renders_title_header_and_rows_without_selector() {
        let session = demo("alpha");
        let rendered = render_session_list_lines(&[session], 120).join("\n");

        assert!(!rendered.contains("agentd - "));
        assert!(rendered.contains("RUN"));
        assert!(rendered.contains("NAME"));
        assert!(rendered.contains("CWD"));
        assert!(!rendered.contains("BRANCH"));
        assert!(rendered.contains("/tmp/alpha"));
        assert!(rendered.contains("●"));
        assert!(!rendered.contains("› "));
    }

    #[test]
    fn ordered_sessions_keep_active_sessions_first_then_attention() {
        let now = Utc::now();
        let mut running = demo("running");
        running.updated_at = now - Duration::minutes(2);
        let mut creating = demo_with("creating", SessionStatus::Creating);
        creating.updated_at = now - Duration::minutes(1);
        let mut failed = demo_with("failed", SessionStatus::Failed);
        failed.updated_at = now;
        let mut exited = demo_with("exited", SessionStatus::Exited);
        exited.updated_at = now;
        let mut needs_action = demo_with("needs-action", SessionStatus::Exited);
        needs_action.attention = AttentionLevel::Action;
        needs_action.updated_at = now - Duration::minutes(5);

        let sessions = [exited, failed, running, needs_action, creating];
        let ordered = ordered_sessions(&sessions);

        assert_eq!(
            ordered.iter().map(|session| session.session_id.as_str()).collect::<Vec<_>>(),
            vec!["creating", "running", "failed", "needs-action", "exited"]
        );
    }

    #[test]
    fn session_list_uses_single_row_and_dynamic_widths() {
        let mut session = demo("alpha");
        session.session_id =
            "a-very-long-session-name-that-should-be-truncated-in-the-fixed-width-column"
                .to_string();
        session.cwd =
            "/srv/checkouts/a-very-long-directory-name/that-should-be-truncated".to_string();
        session.attention_summary =
            Some("needs manual review because a long follow-up summary is present".to_string());

        let wide = render_session_list_lines(&[session.clone()], 120).join("\n");
        let narrow = render_session_list_lines(&[session], 80).join("\n");

        assert_eq!(wide.lines().count(), 2);
        assert!(!wide.contains("needs manual review because a long follow-up summary is present"));
        assert!(wide.contains("a-very"));
        assert!(wide.contains("that-should-be-truncated"));
        assert!(narrow.contains("..."));
        assert!(narrow.contains("that-should-be-truncated"));
    }

    #[test]
    fn session_list_shows_empty_state() {
        let rendered = render_session_list_lines(&[], 120).join("\n");

        assert!(rendered.contains("No sessions."));
        assert!(rendered.contains("RUN"));
    }

    #[test]
    fn session_list_does_not_render_legend() {
        let rendered = render_session_list_lines(
            &[
                demo_with("alpha", SessionStatus::Running),
                demo_with("beta", SessionStatus::UnknownRecovered),
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
        assert_eq!(
            wide.chars().count(),
            render_session_list_header_content(wide_layout).chars().count()
        );
        assert_eq!(narrow, render_session_list_header_content(narrow_layout));
        assert_eq!(
            narrow.chars().count(),
            render_session_list_header_content(narrow_layout).chars().count()
        );
    }

    #[test]
    fn host_picker_legend_is_hidden_in_submenus() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::CreateAgentSelect { selected: 0 },
            toast: None,
        };

        let rendered = picker.render_lines(200, true).join("\n");
        assert!(!rendered.contains(" • "));
        assert!(!rendered.contains("running"));
    }

    #[test]
    fn host_picker_session_row_uses_exit_time_for_finished_sessions() {
        let mut session = demo_with("alpha", SessionStatus::Exited);
        let created_at = Utc::now() - Duration::hours(5);
        session.created_at = created_at;
        session.exited_at = Some(created_at + Duration::minutes(90));

        let rendered = strip_ansi(&render_host_picker_session_row(&session, 120, false));

        assert!(rendered.contains("1h"));
        assert!(rendered.contains("alpha"));
    }

    #[test]
    fn host_picker_session_row_uses_fixed_width_columns_and_truncation() {
        let mut session = demo("alpha");
        session.created_at = Utc::now() - Duration::minutes(23);
        session.session_id =
            "a-very-long-session-name-that-should-be-truncated-in-the-fixed-width-column"
                .to_string();
        session.cwd =
            "/srv/checkouts/a-very-long-directory-name/that-should-be-truncated".to_string();

        let wide = strip_ansi(&render_host_picker_session_row(&session, 120, false));
        let narrow = strip_ansi(&render_host_picker_session_row(&session, 60, false));

        assert!(wide.contains("23m"));
        assert!(wide.contains("a-very"));
        assert!(wide.contains("directory-name/that-should-be-truncated"));
        assert!(narrow.contains("23m"));
        assert!(narrow.contains("...") && narrow.trim_end().ends_with("truncated"));
    }

    #[test]
    fn host_picker_create_row_aligns_with_elapsed_column() {
        let mut session = demo("alpha");
        session.created_at = Utc::now() - Duration::minutes(23);

        let session_row = strip_ansi(&render_host_picker_session_row(&session, 120, false));
        let create_row =
            strip_ansi(&render_host_picker_create_row("Create new session", 120, false));
        let session_column = session_row
            .split("23m")
            .next()
            .expect("session row should include elapsed time")
            .chars()
            .count();
        let create_column = create_row
            .split("Create new session")
            .next()
            .expect("create row should include label")
            .chars()
            .count();

        assert_eq!(session_column, create_column);
    }

    #[test]
    fn host_picker_create_row_truncates_query_text() {
        let rendered =
            strip_ansi(&render_host_picker_create_row("Create new session: beta", 20, false));

        assert!(rendered.trim_end().ends_with("..."));
    }

    #[test]
    fn picker_toast_flattens_embedded_newlines() {
        assert_eq!(
            sanitize_picker_message("merge would conflict\nrun:\n  git status"),
            "merge would conflict run: git status"
        );
    }

    #[test]
    fn picker_error_toast_renders_in_red() {
        let rendered =
            render_host_picker_toast_line(&PickerToast::error("merge blocked".to_string()), 120);

        assert!(rendered.starts_with(HOST_PICKER_STATUS_RED_FG));
        assert!(rendered.contains("error: merge blocked"));
        assert!(rendered.ends_with(ANSI_RESET));
    }

    #[test]
    fn browse_mode_renders_error_toast_on_single_line() {
        let picker = SessionPicker {
            paths: test_paths(),
            sessions: vec![demo("alpha")],
            default_agent: Some("codex".to_string()),
            create_agents: vec!["claude".to_string(), "codex".to_string()],
            composer: default_composer(),
            mode: PickerMode::Browse,
            toast: Some(PickerToast::error("merge would conflict\nrun:\n  git status".to_string())),
        };

        let rendered = picker.render_lines(200, true).join("\n");
        assert!(rendered.contains("error: merge would conflict run: git status"));
        assert!(!rendered.contains("error: merge would conflict\nrun:\n  git status"));
    }

    #[test]
    fn format_path_cell_keeps_the_end_of_long_paths() {
        assert_eq!(format_path_cell("/a/b", 6), "/a/b  ");
        assert_eq!(format_path_cell("/srv/repo/checkout", 10), "...heckout");
        assert_eq!(format_path_cell("/srv/repo", 3), "epo");
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

    fn demo(session_id: &str) -> SessionRecord {
        demo_with(session_id, SessionStatus::Running)
    }

    fn demo_with(session_id: &str, status: SessionStatus) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: session_id.to_string(),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            cwd: format!("/tmp/{session_id}"),
            status,
            worker_pid: Some(123),
            agent_pid: Some(456),
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
            sessions_dir: root.join("sessions"),
            root,
        }
    }

    fn default_composer() -> PickerComposer {
        PickerComposer { query: String::new(), selected: 0 }
    }
}
