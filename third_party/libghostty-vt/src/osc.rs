//! Handling OSC (Operating System Command) escape sequences.

use std::marker::PhantomData;

use crate::{
    alloc::{Allocator, Object},
    error::{Result, from_result},
    ffi,
};

/// OSC (Operating System Command) sequence parser and command handling.
///
/// The parser operates in a streaming fashion, processing input byte-by-byte
/// to handle OSC sequences that may arrive in fragments across multiple reads.
/// This interface makes it easy to integrate into most environments and avoids
/// over-allocating buffers.
#[derive(Debug)]
pub struct Parser<'alloc>(Object<'alloc, ffi::GhosttyOscParser>);

impl<'alloc> Parser<'alloc> {
    /// Create a new OSC parser.
    pub fn new() -> Result<Self> {
        // SAFETY: A NULL allocator is always valid
        unsafe { Self::new_inner(std::ptr::null()) }
    }

    /// Create a new OSC parser with a custom allocator.
    ///
    /// See the [crate-level documentation](crate#memory-management-and-lifetimes)
    /// regarding custom memory management and lifetimes.
    pub fn new_with_alloc<'ctx: 'alloc, Ctx>(alloc: &'alloc Allocator<'ctx, Ctx>) -> Result<Self> {
        // SAFETY: Borrow checking should forbid invalid allocators
        unsafe { Self::new_inner(alloc.to_raw()) }
    }

    unsafe fn new_inner(alloc: *const ffi::GhosttyAllocator) -> Result<Self> {
        let mut raw: ffi::GhosttyOscParser_ptr = std::ptr::null_mut();
        let result = unsafe { ffi::ghostty_osc_new(alloc, &raw mut raw) };
        from_result(result)?;
        Ok(Self(Object::new(raw)?))
    }

    /// Reset an OSC parser instance to its initial state.
    ///
    /// Resets the parser state, clearing any partially parsed OSC sequences
    /// and returning the parser to its initial state. This is useful for
    /// reusing a parser instance or recovering from parse errors.
    pub fn reset(&mut self) {
        unsafe { ffi::ghostty_osc_reset(self.0.as_raw()) }
    }

    /// Parse the next byte in an OSC sequence.
    ///
    /// Processes a single byte as part of an OSC sequence. The parser maintains
    /// internal state to track the progress through the sequence. Call this
    /// function for each byte in the sequence data.
    ///
    /// When finished pumping the parser with bytes, call [`Parser::end`] to
    /// get the final result.
    pub fn next_byte(&mut self, byte: u8) {
        unsafe { ffi::ghostty_osc_next(self.0.as_raw(), byte) }
    }

    /// Finalize OSC parsing and retrieve the parsed command.
    ///
    /// Call this function after feeding all bytes of an OSC sequence to the parser
    /// using [`Parser::next_byte`] with the exception of the terminating character
    /// (ESC or ST). This function finalizes the parsing process and returns the
    /// parsed OSC command. Invalid commands will return a command with type
    /// [`CommandType::Invalid`].
    ///
    /// The terminator parameter specifies the byte that terminated the OSC
    /// sequence (typically 0x07 for BEL or 0x5C for ST after ESC).
    /// This information is preserved in the parsed command so that responses
    /// can use the same terminator format for better compatibility with the
    /// calling program. For commands that do not require a response, this
    /// parameter is ignored and the resulting command will not retain the
    /// terminator information.
    #[expect(clippy::missing_panics_doc, reason = "internal invariant")]
    pub fn end<'p>(&'p mut self, terminator: u8) -> Command<'p, 'alloc> {
        let raw = unsafe { ffi::ghostty_osc_end(self.0.as_raw(), terminator) };
        Command {
            inner: Object::new(raw).expect("command must not be null"),
            _parser: PhantomData,
        }
    }
}

impl Drop for Parser<'_> {
    fn drop(&mut self) {
        unsafe { ffi::ghostty_osc_free(self.0.as_raw()) }
    }
}

/// A parsed OSC (Operating System Command) command.
///
/// The command can be queried for its type and associated data.
#[derive(Debug)]
pub struct Command<'p, 'alloc> {
    inner: Object<'alloc, ffi::GhosttyOscCommand>,
    _parser: PhantomData<&'p Parser<'alloc>>,
}

impl Command<'_, '_> {
    /// Get the type of an OSC command.
    ///
    /// This can be used to determine what kind of command was parsed and
    /// what data might be available from it.
    #[must_use]
    pub fn command_type(&self) -> CommandType {
        CommandType::try_from(unsafe { ffi::ghostty_osc_command_type(self.inner.as_raw()) })
            .unwrap_or_default()
    }
}

/// Type of an OSC command.
#[expect(missing_docs, reason = "missing upstream docs")]
#[repr(u32)]
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq, int_enum::IntEnum)]
pub enum CommandType {
    #[default]
    Invalid = ffi::GhosttyOscCommandType_GHOSTTY_OSC_COMMAND_INVALID,
}
