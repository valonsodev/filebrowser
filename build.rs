use std::process::Command;
fn main() {
    println!("cargo:rerun-if-changed=.git/HEAD");
    println!("cargo:rerun-if-changed=.git/refs");
    for (key, command, args) in [
        ("GIT_COMMIT", "git", vec!["rev-parse", "--short", "HEAD"]),
        ("BUILD_DATE", "date", vec!["-u", "+%Y-%m-%dT%H:%M:%SZ"]),
    ] {
        println!("cargo:rerun-if-env-changed={key}");
        let value = std::env::var(key)
            .ok()
            .or_else(|| {
                let output = Command::new(command).args(args).output().ok()?;
                output
                    .status
                    .success()
                    .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
            })
            .unwrap_or_else(|| "unknown".into());
        println!("cargo:rustc-env={key}={value}");
    }
}
