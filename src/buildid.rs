//! Binary identity is computed once, before a development executable can disappear.
use sha2::{Digest, Sha256};
use std::{io::Read, sync::OnceLock};
pub fn id() -> &'static str {
    static ID: OnceLock<String> = OnceLock::new();
    ID.get_or_init(|| {
        (|| -> std::io::Result<String> {
            let mut file = std::fs::File::open(std::env::current_exe()?)?;
            let mut hash = Sha256::new();
            let mut buffer = [0; 64 * 1024];
            loop {
                let n = file.read(&mut buffer)?;
                if n == 0 {
                    break;
                }
                hash.update(&buffer[..n]);
            }
            Ok(format!("{:x}", hash.finalize())[..12].into())
        })()
        .unwrap_or_else(|_| "unknown".into())
    })
}
pub fn version() -> String {
    let version = option_env!("STAVLOS_BUILD_VERSION").unwrap_or("devel");
    let mut output = format!("stavlos {version}\n");
    let revision = option_env!("STAVLOS_BUILD_COMMIT").unwrap_or("");
    if !revision.is_empty() {
        output.push_str(&format!("commit  {}", &revision[..revision.len().min(12)]));
        let time = option_env!("STAVLOS_BUILD_TIME").unwrap_or("");
        if !time.is_empty() {
            output.push_str(&format!(", {time}"));
        }
        if option_env!("STAVLOS_BUILD_MODIFIED") == Some("true") {
            output.push_str(", with uncommitted changes");
        }
        output.push('\n');
    }
    output.push_str(&format!(
        "rust    {} {}/{}\nbuild   {}\n",
        option_env!("STAVLOS_RUST_VERSION").unwrap_or("rustc"),
        std::env::consts::OS,
        std::env::consts::ARCH,
        id()
    ));
    output
}
