use std::{ffi::c_void, ptr, slice};

use anyhow::{Result, anyhow, bail};

const GHOSTTY_SUCCESS: i32 = 0;

pub trait TerminalStateEngine: Send {
    fn feed(&mut self, data: &[u8]) -> Result<()>;
    fn snapshot(&mut self) -> Result<Vec<u8>>;
}

pub struct GhosttyTerminalState {
    terminal: GhosttyTerminal,
    formatter: GhosttyFormatter,
}

impl GhosttyTerminalState {
    pub fn new(cols: u16, rows: u16, max_scrollback: usize) -> Result<Self> {
        let mut terminal = ptr::null_mut();
        let result = unsafe {
            ghostty_terminal_new(
                ptr::null(),
                &mut terminal,
                GhosttyTerminalOptions {
                    cols,
                    rows,
                    max_scrollback,
                },
            )
        };
        ensure_success(result, "ghostty_terminal_new")?;

        let mut formatter = ptr::null_mut();
        let result = unsafe {
            ghostty_formatter_terminal_new(
                ptr::null(),
                &mut formatter,
                terminal,
                GhosttyFormatterTerminalOptions {
                    size: std::mem::size_of::<GhosttyFormatterTerminalOptions>(),
                    emit: GhosttyFormatterFormat::Vt,
                    unwrap: false,
                    trim: false,
                    extra: GhosttyFormatterTerminalExtra {
                        size: std::mem::size_of::<GhosttyFormatterTerminalExtra>(),
                        palette: true,
                        modes: true,
                        scrolling_region: true,
                        tabstops: true,
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
        if let Err(err) = ensure_success(result, "ghostty_formatter_terminal_new") {
            unsafe { ghostty_terminal_free(terminal) };
            return Err(err);
        }

        Ok(Self {
            terminal,
            formatter,
        })
    }
}

impl TerminalStateEngine for GhosttyTerminalState {
    fn feed(&mut self, data: &[u8]) -> Result<()> {
        unsafe { ghostty_terminal_vt_write(self.terminal, data.as_ptr(), data.len()) };
        Ok(())
    }

    fn snapshot(&mut self) -> Result<Vec<u8>> {
        let mut ptr = ptr::null_mut();
        let mut len = 0usize;
        let result = unsafe {
            ghostty_formatter_format_alloc(self.formatter, ptr::null(), &mut ptr, &mut len)
        };
        ensure_success(result, "ghostty_formatter_format_alloc")?;
        if ptr.is_null() && len != 0 {
            bail!("ghostty formatter returned a null buffer for a non-empty snapshot");
        }

        let bytes = unsafe { slice::from_raw_parts(ptr, len) }.to_vec();
        unsafe { libc_free(ptr.cast()) };
        Ok(bytes)
    }
}

impl Drop for GhosttyTerminalState {
    fn drop(&mut self) {
        unsafe {
            ghostty_formatter_free(self.formatter);
            ghostty_terminal_free(self.terminal);
        }
    }
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
        _ => "unknown error",
    };
    Err(anyhow!("{operation} failed: {message} ({result})"))
}

type GhosttyTerminal = *mut c_void;
type GhosttyFormatter = *mut c_void;
type GhosttyAllocator = c_void;

#[repr(C)]
struct GhosttyTerminalOptions {
    cols: u16,
    rows: u16,
    max_scrollback: usize,
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

unsafe extern "C" {
    fn ghostty_terminal_new(
        allocator: *const GhosttyAllocator,
        terminal: *mut GhosttyTerminal,
        options: GhosttyTerminalOptions,
    ) -> i32;
    fn ghostty_terminal_free(terminal: GhosttyTerminal);
    fn ghostty_terminal_vt_write(terminal: GhosttyTerminal, data: *const u8, len: usize);

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

    fn free(ptr: *mut c_void);
}

unsafe fn libc_free(ptr: *mut c_void) {
    if !ptr.is_null() {
        unsafe { free(ptr) };
    }
}
