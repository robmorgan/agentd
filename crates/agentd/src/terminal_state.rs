use std::{
    ffi::{c_char, c_void},
    ptr, slice,
};

use anyhow::{Context, Result, anyhow, bail};

const GHOSTTY_SUCCESS: i32 = 0;
const GHOSTTY_OUT_OF_SPACE: i32 = -3;

pub trait TerminalStateEngine: Send {
    fn feed(&mut self, data: &[u8], effects_enabled: bool) -> Result<Vec<u8>>;
    fn resize(
        &mut self,
        cols: u16,
        rows: u16,
        cell_width_px: u32,
        cell_height_px: u32,
    ) -> Result<()>;
    fn vt_snapshot(&mut self) -> Result<Vec<u8>>;
    fn format_plain(&mut self) -> Result<String>;
    fn format_vt(&mut self) -> Result<String>;
}

pub struct GhosttyTerminalState {
    terminal: GhosttyTerminal,
    plain_formatter: GhosttyFormatter,
    vt_formatter: GhosttyFormatter,
    size: GhosttySizeReportSize,
    query_scan_tail: Vec<u8>,
}

impl GhosttyTerminalState {
    pub fn new(cols: u16, rows: u16, max_scrollback: usize) -> Result<Self> {
        let mut terminal = ptr::null_mut();
        let result = unsafe {
            ghostty_terminal_new(
                ptr::null(),
                &mut terminal,
                GhosttyTerminalOptions { cols, rows, max_scrollback },
            )
        };
        ensure_success(result, "ghostty_terminal_new")?;

        let plain_formatter = match new_formatter(terminal, GhosttyFormatterFormat::Plain) {
            Ok(formatter) => formatter,
            Err(err) => {
                unsafe { ghostty_terminal_free(terminal) };
                return Err(err);
            }
        };
        let vt_formatter = match new_formatter(terminal, GhosttyFormatterFormat::Vt) {
            Ok(formatter) => formatter,
            Err(err) => {
                unsafe {
                    ghostty_formatter_free(plain_formatter);
                    ghostty_terminal_free(terminal);
                }
                return Err(err);
            }
        };

        Ok(Self {
            terminal,
            plain_formatter,
            vt_formatter,
            size: GhosttySizeReportSize { rows, columns: cols, cell_width: 0, cell_height: 0 },
            query_scan_tail: Vec::new(),
        })
    }
}

impl TerminalStateEngine for GhosttyTerminalState {
    fn feed(&mut self, data: &[u8], effects_enabled: bool) -> Result<Vec<u8>> {
        unsafe { ghostty_terminal_vt_write(self.terminal, data.as_ptr(), data.len()) };
        if !effects_enabled {
            self.query_scan_tail.clear();
            return Ok(Vec::new());
        }
        self.collect_effect_responses(data)
    }

    fn resize(
        &mut self,
        cols: u16,
        rows: u16,
        cell_width_px: u32,
        cell_height_px: u32,
    ) -> Result<()> {
        self.size = GhosttySizeReportSize {
            rows,
            columns: cols,
            cell_width: cell_width_px,
            cell_height: cell_height_px,
        };
        let result = unsafe { ghostty_terminal_resize(self.terminal, cols, rows) };
        ensure_success(result, "ghostty_terminal_resize")
    }

    fn vt_snapshot(&mut self) -> Result<Vec<u8>> {
        format_terminal(self.vt_formatter)
    }

    fn format_plain(&mut self) -> Result<String> {
        Ok(String::from_utf8_lossy(&format_terminal(self.plain_formatter)?).into_owned())
    }

    fn format_vt(&mut self) -> Result<String> {
        Ok(String::from_utf8_lossy(&format_terminal(self.vt_formatter)?).into_owned())
    }
}

impl GhosttyTerminalState {
    fn collect_effect_responses(&mut self, data: &[u8]) -> Result<Vec<u8>> {
        const MODE_QUERY: &[u8] = b"\x1b[?7$p";
        const SIZE_QUERY: &[u8] = b"\x1b[18t";
        const MAX_QUERY_LEN: usize = MODE_QUERY.len();

        let prefix_len = self.query_scan_tail.len();
        let mut combined = Vec::with_capacity(prefix_len + data.len());
        combined.extend_from_slice(&self.query_scan_tail);
        combined.extend_from_slice(data);

        let mut responses = Vec::new();
        for idx in 0..combined.len() {
            if combined[idx..].starts_with(MODE_QUERY) && idx + MODE_QUERY.len() > prefix_len {
                responses.extend_from_slice(&self.encode_wraparound_mode_report()?);
            }
            if combined[idx..].starts_with(SIZE_QUERY) && idx + SIZE_QUERY.len() > prefix_len {
                responses.extend_from_slice(&self.encode_size_report()?);
            }
        }

        let keep = combined.len().min(MAX_QUERY_LEN - 1);
        self.query_scan_tail.clear();
        self.query_scan_tail.extend_from_slice(&combined[combined.len() - keep..]);
        Ok(responses)
    }

    fn encode_wraparound_mode_report(&self) -> Result<Vec<u8>> {
        let mut enabled = false;
        let result = unsafe {
            ghostty_terminal_mode_get(self.terminal, GHOSTTY_MODE_WRAPAROUND, &mut enabled)
        };
        ensure_success(result, "ghostty_terminal_mode_get(wraparound)")?;
        let state =
            if enabled { GhosttyModeReportState::Set } else { GhosttyModeReportState::Reset };
        encode_with_len(|buf, len, written| unsafe {
            ghostty_mode_report_encode(GHOSTTY_MODE_WRAPAROUND, state, buf, len, written)
        })
        .context("failed to encode wraparound mode report")
    }

    fn encode_size_report(&self) -> Result<Vec<u8>> {
        encode_with_len(|buf, len, written| unsafe {
            ghostty_size_report_encode(GhosttySizeReportStyle::Csi18T, self.size, buf, len, written)
        })
        .context("failed to encode size report")
    }
}

impl Drop for GhosttyTerminalState {
    fn drop(&mut self) {
        unsafe {
            ghostty_formatter_free(self.plain_formatter);
            ghostty_formatter_free(self.vt_formatter);
            ghostty_terminal_free(self.terminal);
        }
    }
}

fn new_formatter(
    terminal: GhosttyTerminal,
    emit: GhosttyFormatterFormat,
) -> Result<GhosttyFormatter> {
    let mut formatter = ptr::null_mut();
    let result = unsafe {
        ghostty_formatter_terminal_new(
            ptr::null(),
            &mut formatter,
            terminal,
            GhosttyFormatterTerminalOptions {
                size: std::mem::size_of::<GhosttyFormatterTerminalOptions>(),
                emit,
                unwrap: false,
                trim: false,
                extra: GhosttyFormatterTerminalExtra {
                    size: std::mem::size_of::<GhosttyFormatterTerminalExtra>(),
                    palette: true,
                    modes: true,
                    scrolling_region: true,
                    tabstops: false,
                    pwd: false,
                    keyboard: true,
                    screen: GhosttyFormatterScreenExtra {
                        size: std::mem::size_of::<GhosttyFormatterScreenExtra>(),
                        cursor: true,
                        style: true,
                        hyperlink: true,
                        protection: true,
                        kitty_keyboard: true,
                        charsets: true,
                    },
                },
            },
        )
    };
    ensure_success(result, "ghostty_formatter_terminal_new")?;
    Ok(formatter)
}

fn format_terminal(formatter: GhosttyFormatter) -> Result<Vec<u8>> {
    let mut ptr = ptr::null_mut();
    let mut len = 0usize;
    let result =
        unsafe { ghostty_formatter_format_alloc(formatter, ptr::null(), &mut ptr, &mut len) };
    ensure_success(result, "ghostty_formatter_format_alloc")?;
    if ptr.is_null() && len != 0 {
        bail!("ghostty formatter returned a null buffer for a non-empty snapshot");
    }

    let bytes = unsafe { slice::from_raw_parts(ptr, len) }.to_vec();
    unsafe { free(ptr.cast()) };
    Ok(bytes)
}

unsafe impl Send for GhosttyTerminalState {}

fn encode_with_len(
    mut encode: impl FnMut(*mut c_char, usize, *mut usize) -> i32,
) -> Result<Vec<u8>> {
    let mut written = 0usize;
    let result = encode(ptr::null_mut(), 0, &mut written);
    if result != GHOSTTY_OUT_OF_SPACE {
        ensure_success(result, "ghostty encode length query")?;
    }

    let mut buf = vec![0u8; written];
    let result = encode(buf.as_mut_ptr().cast(), buf.len(), &mut written);
    ensure_success(result, "ghostty encode")?;
    buf.truncate(written);
    Ok(buf)
}

fn ensure_success(result: i32, operation: &str) -> Result<()> {
    if result == GHOSTTY_SUCCESS {
        return Ok(());
    }

    let message = match result {
        -1 => "out of memory",
        -2 => "invalid value",
        -3 => "out of space",
        -4 => "no value",
        _ => "unknown error",
    };
    Err(anyhow!("{operation} failed: {message} ({result})"))
}

type GhosttyTerminal = *mut c_void;
type GhosttyFormatter = *mut c_void;
type GhosttyAllocator = c_void;
type GhosttyMode = u16;

const GHOSTTY_MODE_WRAPAROUND: GhosttyMode = 7;

#[repr(C)]
struct GhosttyTerminalOptions {
    cols: u16,
    rows: u16,
    max_scrollback: usize,
}

#[repr(C)]
#[derive(Clone, Copy)]
struct GhosttySizeReportSize {
    rows: u16,
    columns: u16,
    cell_width: u32,
    cell_height: u32,
}

#[repr(C)]
struct GhosttyFormatterScreenExtra {
    size: usize,
    cursor: bool,
    style: bool,
    hyperlink: bool,
    protection: bool,
    kitty_keyboard: bool,
    charsets: bool,
}

#[repr(C)]
struct GhosttyFormatterTerminalExtra {
    size: usize,
    palette: bool,
    modes: bool,
    scrolling_region: bool,
    tabstops: bool,
    pwd: bool,
    keyboard: bool,
    screen: GhosttyFormatterScreenExtra,
}

#[repr(C)]
struct GhosttyFormatterTerminalOptions {
    size: usize,
    emit: GhosttyFormatterFormat,
    unwrap: bool,
    trim: bool,
    extra: GhosttyFormatterTerminalExtra,
}

#[allow(dead_code)]
#[repr(C)]
enum GhosttyFormatterFormat {
    Plain = 0,
    Vt = 1,
    Html = 2,
}

#[repr(C)]
#[derive(Clone, Copy)]
enum GhosttyModeReportState {
    #[allow(dead_code)]
    NotRecognized = 0,
    Set = 1,
    Reset = 2,
    #[allow(dead_code)]
    PermanentlySet = 3,
    #[allow(dead_code)]
    PermanentlyReset = 4,
}

#[repr(C)]
#[derive(Clone, Copy)]
enum GhosttySizeReportStyle {
    #[allow(dead_code)]
    Mode2048 = 0,
    #[allow(dead_code)]
    Csi14T = 1,
    #[allow(dead_code)]
    Csi16T = 2,
    Csi18T = 3,
}

unsafe extern "C" {
    fn ghostty_terminal_new(
        allocator: *const GhosttyAllocator,
        terminal: *mut GhosttyTerminal,
        options: GhosttyTerminalOptions,
    ) -> i32;
    fn ghostty_terminal_free(terminal: GhosttyTerminal);
    fn ghostty_terminal_vt_write(terminal: GhosttyTerminal, data: *const u8, len: usize);
    fn ghostty_terminal_resize(terminal: GhosttyTerminal, cols: u16, rows: u16) -> i32;
    fn ghostty_terminal_mode_get(
        terminal: GhosttyTerminal,
        mode: GhosttyMode,
        out_value: *mut bool,
    ) -> i32;

    fn ghostty_formatter_terminal_new(
        allocator: *const GhosttyAllocator,
        formatter: *mut GhosttyFormatter,
        terminal: GhosttyTerminal,
        options: GhosttyFormatterTerminalOptions,
    ) -> i32;
    fn ghostty_formatter_format_alloc(
        formatter: GhosttyFormatter,
        allocator: *const GhosttyAllocator,
        out_ptr: *mut *mut u8,
        out_len: *mut usize,
    ) -> i32;
    fn ghostty_formatter_free(formatter: GhosttyFormatter);
    fn ghostty_mode_report_encode(
        mode: GhosttyMode,
        state: GhosttyModeReportState,
        buf: *mut c_char,
        buf_len: usize,
        out_written: *mut usize,
    ) -> i32;
    fn ghostty_size_report_encode(
        style: GhosttySizeReportStyle,
        size: GhosttySizeReportSize,
        buf: *mut c_char,
        buf_len: usize,
        out_written: *mut usize,
    ) -> i32;
    fn free(ptr: *mut c_void);
}

#[cfg(test)]
mod tests {
    use super::{GhosttyTerminalState, TerminalStateEngine};

    #[test]
    fn feed_returns_mode_query_response_when_effects_enabled() {
        let mut state = GhosttyTerminalState::new(80, 24, 0).unwrap();
        let response = state.feed(b"\x1b[?7$p", true).unwrap();
        assert_eq!(response, b"\x1b[?7;1$y");
    }

    #[test]
    fn feed_returns_size_query_response_when_effects_enabled() {
        let mut state = GhosttyTerminalState::new(80, 24, 0).unwrap();
        state.resize(80, 24, 8, 16).unwrap();
        let response = state.feed(b"\x1b[18t", true).unwrap();
        assert_eq!(response, b"\x1b[8;24;80t");
    }

    #[test]
    fn feed_suppresses_effects_when_disabled() {
        let mut state = GhosttyTerminalState::new(80, 24, 0).unwrap();
        let response = state.feed(b"\x1b[?7$p", false).unwrap();
        assert!(response.is_empty());
    }

    #[test]
    fn feed_recovers_split_query_across_calls() {
        let mut state = GhosttyTerminalState::new(80, 24, 0).unwrap();
        let first = state.feed(b"\x1b[", true).unwrap();
        assert!(first.is_empty());
        let second = state.feed(b"18t", true).unwrap();
        assert_eq!(second, b"\x1b[8;24;80t");
    }
}
