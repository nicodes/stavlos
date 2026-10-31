use stavlos_api::{
    config::Config,
    control::Control,
    engine::Engine,
    error::{Result, fail},
};
use std::time::Duration;
use tokio::io::AsyncWriteExt;
#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("stavlos API: {}", error.message);
        std::process::exit(1);
    }
}
async fn run() -> Result<()> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    if args.iter().any(|s| s == "--help" || s == "-h") {
        println!(
            "stavlos-api --data-dir ABSOLUTE_PATH [--port 4999] [--hosts proxy.example] [--parent-pid PID]"
        );
        return Ok(());
    }
    if args.first().is_some_and(|s| s == "oauth-callback") {
        return stavlos_api::oauth::run(&args[1..]).await;
    }
    let config = Config::parse(&args)?;
    let parent = config.parent_pid;
    let (output, mut receiving) = tokio::sync::mpsc::channel::<serde_json::Value>(16);
    let engine = Engine::new(output.clone(), config.data.clone());
    let control = Control::new(config, engine);
    let mut writer = tokio::spawn(async move {
        let mut stdout = tokio::io::BufWriter::new(tokio::io::stdout());
        while let Some(value) = receiving.recv().await {
            let mut raw = serde_json::to_vec(&value)
                .map_err(|_| std::io::Error::other("control encoding failed"))?;
            raw.push(b'\n');
            tokio::time::timeout(Duration::from_secs(5), async {
                stdout.write_all(&raw).await?;
                stdout.flush().await
            })
            .await
            .map_err(|_| std::io::Error::other("control reader stalled"))??;
        }
        Ok::<_, std::io::Error>(())
    });
    output
        .send(Engine::ready_message())
        .await
        .map_err(|_| fail(503, "control output unavailable"))?;
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    let result = tokio::select! {
     result=stavlos_api::control::read(control.clone(),output)=>result,
     _=tokio::signal::ctrl_c()=>Ok(()),_=terminate.recv()=>Ok(()),
     _=async{if let Some(pid)=parent{loop{tokio::time::sleep(Duration::from_millis(250)).await;if unsafe{libc::getppid()}as u32!=pid{break;}}}else{std::future::pending::<()>().await;}}=>Ok(()),
     _=&mut writer=>Err(fail(503,"control output ended")),
    };
    control.disable().await;
    if !writer.is_finished() {
        writer.abort();
        let _ = writer.await;
    }
    result
}
