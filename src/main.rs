use anyhow::{Context, Result};
use stavlos::{app, paths::Paths};
use std::{
    ffi::OsString,
    path::{Path, PathBuf},
    process::ExitCode,
    sync::Arc,
    time::Duration,
};
use tokio_util::sync::CancellationToken;
fn main() -> ExitCode {
    let mut arguments = std::env::args_os();
    let name = arguments.next().unwrap_or_default();
    let arguments = arguments.collect::<Vec<OsString>>();
    let basename = Path::new(&name).file_name().unwrap_or_default();
    // Both helpers must dispatch before any owner threads or process hardening.
    if basename == stavlos::sandbox::HELPER_ARG0 {
        return match stavlos::sandbox::entry(&arguments) {
            Ok(code) => ExitCode::from(code as u8),
            Err(error) => {
                eprintln!("stavlos sandbox: {error}");
                ExitCode::FAILURE
            }
        };
    }
    if basename == stavlos::nativeapi::HELPER_ARG0 {
        return api();
    }
    match run(arguments) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("stavlos: {error:#}");
            ExitCode::FAILURE
        }
    }
}
fn runtime() -> Result<tokio::runtime::Runtime> {
    Ok(tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?)
}
fn api() -> ExitCode {
    let runtime = match runtime() {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("stavlos API: {error}");
            return ExitCode::FAILURE;
        }
    };
    let result = runtime.block_on(stavlos_api::entry::run());
    runtime.shutdown_timeout(Duration::from_secs(1));
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("stavlos API: {}", error.message);
            ExitCode::FAILURE
        }
    }
}
fn run(arguments: Vec<OsString>) -> Result<()> {
    let arguments = arguments
        .into_iter()
        .map(|value| {
            value
                .into_string()
                .map_err(|_| anyhow::anyhow!("CLI arguments must be valid UTF-8"))
        })
        .collect::<Result<Vec<_>>>()?;
    unsafe {
        libc::umask(0o077);
    }
    if app::command(&arguments).0 == "daemon" {
        stavlos::peercred::harden_owner().context("protect owner process")?;
    }
    stavlos::buildid::id();
    let runtime = runtime()?;
    let result=runtime.block_on(async {
        let cancel=CancellationToken::new();let stopping=cancel.clone();let mut terminate=tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
        let signals=tokio::spawn(async move{tokio::select!{_=tokio::signal::ctrl_c()=>{},_=terminate.recv()=>{},_=stopping.cancelled()=>{}}stopping.cancel();});
        let paths=Paths::from_env();let home=std::env::var_os("HOME").map(PathBuf::from).unwrap_or_default();
        let terminal=Arc::new(stavlos::tui::driver::Terminal{paths:paths.clone(),home:home.clone(),cancel:cancel.clone()});let app=app::App{paths,home,terminal,cancel:cancel.clone()};
        let result=app.run(&arguments).await;cancel.cancel();let _=signals.await;result
    });
    runtime.shutdown_timeout(Duration::from_secs(1));
    result
}
