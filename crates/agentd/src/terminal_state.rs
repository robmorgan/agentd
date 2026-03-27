use std::{ffi::c_void, ptr, slice};

use anyhow::{Result, anyhow, bail};

const GHOSTTY_SUCCESS: i32 = 0;

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
    userdata: Box<GhosttyUserData>,
}

struct GhosttyUserData {
    effects_enabled: bool,
    pending_write: Vec<u8>,
    size: GhosttySizeReportSize,
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

        let mut userdata = Box::new(GhosttyUserData {
            effects_enabled: false,
            pending_write: Vec::new(),
            size: GhosttySizeReportSize { rows, columns: cols, cell_width: 0, cell_height: 0 },
        });

        if let Err(err) = configure_effects(terminal, &mut userdata) {
            unsafe { ghostty_terminal_free(terminal) };
            return Err(err);
        }

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

        Ok(Self { terminal, plain_formatter, vt_formatter, userdata })
    }
}

impl TerminalStateEngine for GhosttyTerminalState {
    fn feed(&mut self, data: &[u8], effects_enabled: bool) -> Result<Vec<u8>> {
        self.userdata.effects_enabled = effects_enabled;
        self.userdata.pending_write.clear();
        unsafe { ghostty_terminal_vt_write(self.terminal, data.as_ptr(), data.len()) };
        self.userdata.effects_enabled = false;
        Ok(std::mem::take(&mut self.userdata.pending_write))
    }

    fn resize(
        &mut self,
        cols: u16,
        rows: u16,
        cell_width_px: u32,
        cell_height_px: u32,
    ) -> Result<()> {
        self.userdata.size = GhosttySizeReportSize {
            rows,
            columns: cols,
            cell_width: cell_width_px,
            cell_height: cell_height_px,
        };
        let result = unsafe {
            ghostty_terminal_resize(self.terminal, cols, rows, cell_width_px, cell_height_px)
        };
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

impl Drop for GhosttyTerminalState {
    fn drop(&mut self) {
        unsafe {
            ghostty_formatter_free(self.plain_formatter);
            ghostty_formatter_free(self.vt_formatter);
            ghostty_terminal_free(self.terminal);
        }
    }
}

fn configure_effects(terminal: GhosttyTerminal, userdata: &mut GhosttyUserData) -> Result<()> {
    let userdata_ptr = (userdata as *mut GhosttyUserData).cast::<c_void>();
    let write_pty: GhosttyTerminalWritePtyFn = ghostty_write_pty;
    let size_callback: GhosttyTerminalSizeFn = ghostty_size_callback;

    let result = unsafe {
        ghostty_terminal_set(terminal, GhosttyTerminalOption::Userdata, userdata_ptr.cast())
    };
    ensure_success(result, "ghostty_terminal_set(userdata)")?;

    let result = unsafe {
        ghostty_terminal_set(
            terminal,
            GhosttyTerminalOption::WritePty,
            write_pty as usize as *const c_void,
        )
    };
    ensure_success(result, "ghostty_terminal_set(write_pty)")?;

    let result = unsafe {
        ghostty_terminal_set(
            terminal,
            GhosttyTerminalOption::Size,
            size_callback as usize as *const c_void,
        )
    };
    ensure_success(result, "ghostty_terminal_set(size)")?;

    Ok(())
}

extern "C" fn ghostty_write_pty(
    _terminal: GhosttyTerminal,
    userdata: *mut c_void,
    data: *const u8,
    len: usize,
) {
    if userdata.is_null() || data.is_null() || len == 0 {
        return;
    }
    let userdata = unsafe { &mut *userdata.cast::<GhosttyUserData>() };
    if !userdata.effects_enabled {
        return;
    }
    let bytes = unsafe { slice::from_raw_parts(data, len) };
    userdata.pending_write.extend_from_slice(bytes);
}

extern "C" fn ghostty_size_callback(
    _terminal: GhosttyTerminal,
    userdata: *mut c_void,
    out_size: *mut GhosttySizeReportSize,
) -> bool {
    if userdata.is_null() || out_size.is_null() {
        return false;
    }
    let userdata = unsafe { &mut *userdata.cast::<GhosttyUserData>() };
    if !userdata.effects_enabled {
        return false;
    }
    unsafe { *out_size = userdata.size };
    true
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
    unsafe { ghostty_free(ptr::null(), ptr, len) };
    Ok(bytes)
}

unsafe impl Send for GhosttyTerminalState {}

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
type GhosttyTerminalWritePtyFn = extern "C" fn(GhosttyTerminal, *mut c_void, *const u8, usize);
type GhosttyTerminalSizeFn =
    extern "C" fn(GhosttyTerminal, *mut c_void, *mut GhosttySizeReportSize) -> bool;

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

#[repr(C)]
enum GhosttyTerminalOption {
    Userdata = 0,
    WritePty = 1,
    Size = 6,
}

#[allow(dead_code)]
#[repr(C)]
enum GhosttyFormatterFormat {
    Plain = 0,
    Vt = 1,
    Html = 2,
}

unsafe extern "C" {
    fn ghostty_terminal_new(
        allocator: *const GhosttyAllocator,
        terminal: *mut GhosttyTerminal,
        options: GhosttyTerminalOptions,
    ) -> i32;
    fn ghostty_terminal_free(terminal: GhosttyTerminal);
    fn ghostty_terminal_vt_write(terminal: GhosttyTerminal, data: *const u8, len: usize);
    fn ghostty_terminal_resize(
        terminal: GhosttyTerminal,
        cols: u16,
        rows: u16,
        cell_width_px: u32,
        cell_height_px: u32,
    ) -> i32;
    fn ghostty_terminal_set(
        terminal: GhosttyTerminal,
        option: GhosttyTerminalOption,
        value: *const c_void,
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
    fn ghostty_free(allocator: *const GhosttyAllocator, ptr: *mut u8, len: usize);
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
}
