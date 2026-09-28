use nix::{errno::Errno, libc, sys::signal::kill, unistd::Pid};

pub fn process_exists(pid: Option<u32>) -> bool {
    let Some(pid) = pid else {
        return false;
    };
    if pid == 0 {
        return false;
    }

    match kill(Pid::from_raw(pid as i32), None) {
        Ok(()) => {}
        Err(Errno::ESRCH) => return false,
        Err(_) => return false,
    }

    process_exists_after_signal_check(pid)
}

#[cfg(target_os = "macos")]
fn process_exists_after_signal_check(pid: u32) -> bool {
    match process_is_zombie(pid) {
        Some(is_zombie) => !is_zombie,
        None => false,
    }
}

#[cfg(not(target_os = "macos"))]
fn process_exists_after_signal_check(pid: u32) -> bool {
    match process_is_zombie(pid) {
        Some(is_zombie) => !is_zombie,
        None => true,
    }
}

#[cfg(target_os = "macos")]
fn process_is_zombie(pid: u32) -> Option<bool> {
    let mut info = std::mem::MaybeUninit::<libc::proc_bsdinfo>::zeroed();
    let size = std::mem::size_of::<libc::proc_bsdinfo>() as libc::c_int;
    let rc = unsafe {
        libc::proc_pidinfo(
            pid as libc::c_int,
            libc::PROC_PIDTBSDINFO,
            0,
            info.as_mut_ptr().cast(),
            size,
        )
    };
    if rc != size {
        return None;
    }

    let info = unsafe { info.assume_init() };
    Some(info.pbi_status == libc::SZOMB)
}

#[cfg(not(target_os = "macos"))]
fn process_is_zombie(_pid: u32) -> Option<bool> {
    None
}

#[cfg(test)]
mod tests {
    #[cfg(target_os = "macos")]
    use super::process_is_zombie;

    #[test]
    #[cfg(target_os = "macos")]
    fn current_process_is_not_reported_as_zombie() {
        assert_eq!(process_is_zombie(std::process::id()), Some(false));
    }
}
