use std::{env, path::PathBuf};

fn main() {
    if env::var("DOCS_RS").is_ok() {
        return;
    }

    println!("cargo:rerun-if-env-changed=GHOSTTY_SOURCE_DIR");
    println!("cargo:rerun-if-env-changed=TARGET");
    println!("cargo:rerun-if-env-changed=HOST");

    let ghostty_dir = env::var("GHOSTTY_SOURCE_DIR")
        .map(PathBuf::from)
        .expect("GHOSTTY_SOURCE_DIR must point at a ghostty checkout");
    assert!(
        ghostty_dir.join("build.zig").exists(),
        "GHOSTTY_SOURCE_DIR does not contain build.zig: {}",
        ghostty_dir.display()
    );

    println!("cargo:rerun-if-changed={}", ghostty_dir.join("include").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("src").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("build.zig").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("build.zig.zon").display());

    let lib_dir = ghostty_dir.join("zig-out/lib");
    let include_dir = ghostty_dir.join("zig-out/include");
    assert!(
        lib_dir.join("libghostty-vt.dylib").exists() || lib_dir.join("libghostty-vt.so").exists(),
        "prebuilt libghostty-vt not found under {}",
        lib_dir.display()
    );

    println!("cargo:rustc-link-search=native={}", lib_dir.display());
    println!("cargo:rustc-link-lib=dylib=ghostty-vt");
    println!("cargo:include={}", include_dir.display());
    println!("cargo:rustc-link-arg=-Wl,-rpath,{}", lib_dir.display());
}
