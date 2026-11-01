use crate::{
    client::Client,
    oauth, ownedchild,
    protocol::{self, methods},
};
use anyhow::{Result, bail, ensure};
use std::{
    io::{Read, Write},
    sync::Arc,
    time::Duration,
};
struct Echo(Option<libc::termios>);
impl Echo {
    fn hide() -> Result<Self> {
        if unsafe { libc::isatty(0) } != 1 {
            return Ok(Self(None));
        }
        let mut saved = unsafe { std::mem::zeroed::<libc::termios>() };
        ensure!(
            unsafe { libc::tcgetattr(0, &mut saved) } == 0,
            "cannot read terminal settings"
        );
        let mut hidden = saved;
        hidden.c_lflag &= !(libc::ECHO | libc::ECHONL);
        ensure!(
            unsafe { libc::tcsetattr(0, libc::TCSANOW, &hidden) } == 0,
            "cannot hide terminal input"
        );
        Ok(Self(Some(saved)))
    }
}
impl Drop for Echo {
    fn drop(&mut self) {
        if let Some(saved) = &self.0 {
            unsafe {
                libc::tcsetattr(0, libc::TCSANOW, saved);
            }
        }
    }
}
pub async fn read_line(prompt: &str, secret: bool) -> Result<String> {
    print!("{prompt}");
    std::io::stdout().flush()?;
    let echo = if secret { Echo::hide()? } else { Echo(None) };
    let (sent, received) = tokio::sync::oneshot::channel();
    std::thread::spawn(move || {
        let mut line = String::new();
        let result = std::io::stdin().read_line(&mut line).map(|_| line);
        let _ = sent.send(result);
    });
    let line = received.await??;
    drop(echo);
    if secret {
        println!();
    }
    Ok(line.trim().into())
}
pub fn open_browser(url: &str) {
    if !reqwest::Url::parse(url).is_ok_and(|url| matches!(url.scheme(), "http" | "https")) {
        return;
    }
    let mut command = std::process::Command::new("xdg-open");
    command
        .arg(url)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    if let Ok((mut child, owned)) = ownedchild::spawn(|| {
        let child = command.spawn()?;
        let pid = child.id() as i32;
        Ok((child, pid))
    }) {
        std::thread::spawn(move || {
            let _owned = owned;
            let _ = child.wait();
        });
    }
}
#[derive(Clone)]
struct Pick {
    id: String,
    label: String,
    hint: String,
}
async fn select(title: &str, items: &[Pick]) -> Result<Pick> {
    let mut filter = String::new();
    loop {
        let shown = items
            .iter()
            .filter(|item| {
                filter.is_empty()
                    || format!("{} {}", item.id, item.label)
                        .to_lowercase()
                        .contains(&filter.to_lowercase())
            })
            .collect::<Vec<_>>();
        print!("\n{title}");
        if !filter.is_empty() {
            print!(" (filter: {filter:?})");
        }
        println!();
        for (index, item) in shown.iter().take(15).enumerate() {
            let hint = if item.hint.is_empty() {
                String::new()
            } else {
                format!("  — {}", item.hint)
            };
            println!("  {:2}) {:22}{hint}", index + 1, item.label);
        }
        if shown.len() > 15 {
            println!("  … {} more; type to filter", shown.len() - 15);
        }
        let line = read_line("Number, or text to filter (empty to cancel): ", false).await?;
        ensure!(!line.is_empty(), "cancelled");
        if let Ok(index) = line.parse::<usize>()
            && index > 0
            && index <= 15
            && index <= shown.len()
        {
            return Ok(shown[index - 1].clone());
        }
        if let Some(item) = shown.iter().find(|item| {
            item.id.eq_ignore_ascii_case(&line) || item.label.eq_ignore_ascii_case(&line)
        }) {
            return Ok((*item).clone());
        }
        if shown.len() == 1 && !filter.is_empty() {
            return Ok(shown[0].clone());
        }
        filter = line;
    }
}
pub async fn run(client: Arc<Client>, args: &[String]) -> Result<()> {
    let (sub, args) = args
        .split_first()
        .map(|(sub, args)| (sub.as_str(), args))
        .unwrap_or(("list", &[]));
    match sub {
        "login" | "connect" | "add" => {
            login(&client, args.first().map(String::as_str).unwrap_or("")).await
        }
        "list" | "ls" => {
            let list = client
                .do_method(methods::PROVIDER_LIST, &protocol::None::default())
                .await?;
            let home = std::env::var("HOME").unwrap_or_default();
            let path = if !home.is_empty() {
                list.auth_path
                    .strip_prefix(&home)
                    .map(|rest| format!("~{rest}"))
                    .unwrap_or(list.auth_path)
            } else {
                list.auth_path
            };
            println!("Providers  {path}");
            for provider in list.providers.iter() {
                if provider.connected {
                    println!(
                        "  {:10} {}",
                        provider.name,
                        if provider.account.is_empty() {
                            "connected"
                        } else {
                            &provider.account
                        }
                    );
                } else {
                    println!("  {:10} not connected — {}", provider.name, provider.label);
                }
            }
            Ok(())
        }
        "logout" | "remove" | "rm" => {
            let list = client
                .do_method(methods::PROVIDER_LIST, &protocol::None::default())
                .await?;
            let items = list
                .providers
                .iter()
                .filter(|provider| provider.connected)
                .map(|provider| Pick {
                    id: provider.id.clone(),
                    label: provider.name.clone(),
                    hint: provider.account.clone(),
                })
                .collect::<Vec<_>>();
            ensure!(!items.is_empty(), "no providers connected");
            let wanted = args.join(" ");
            let chosen = if wanted.is_empty() {
                select("Remove credential", &items).await?
            } else {
                items
                    .iter()
                    .find(|item| {
                        item.id.eq_ignore_ascii_case(&wanted)
                            || item.label.eq_ignore_ascii_case(&wanted)
                    })
                    .cloned()
                    .ok_or_else(|| anyhow::anyhow!("no stored credential for {wanted:?}"))?
            };
            client
                .do_method(
                    methods::PROVIDER_DISCONNECT,
                    &protocol::ProviderRef {
                        provider: chosen.id,
                    },
                )
                .await?;
            println!("signed out of {}", chosen.label);
            Ok(())
        }
        _ => bail!("usage: stavlos auth login [provider] | list | logout [provider]"),
    }
}
async fn login(client: &Client, wanted: &str) -> Result<()> {
    let list = client
        .do_method(methods::PROVIDER_LIST, &protocol::None::default())
        .await?;
    let items = list
        .providers
        .iter()
        .map(|provider| Pick {
            id: provider.id.clone(),
            label: provider.name.clone(),
            hint: if provider.connected {
                format!("connected · {}", provider.account)
            } else {
                provider.label.clone()
            },
        })
        .collect::<Vec<_>>();
    let chosen = if wanted.is_empty() {
        select("Sign in with", &items).await?
    } else {
        items.iter().find(|item|item.id.eq_ignore_ascii_case(wanted)||item.label.eq_ignore_ascii_case(wanted)).cloned().ok_or_else(||anyhow::anyhow!("unknown provider {wanted:?}; stavlos supports openai (ChatGPT), xai (Grok), zai (GLM Coding Plan) and kimi (Kimi For Coding)"))?
    };
    let mut method = String::new();
    if let Some(provider) = list
        .providers
        .iter()
        .find(|provider| provider.id == chosen.id)
        && provider.methods.len() > 1
    {
        let methods = provider
            .methods
            .iter()
            .map(|method| Pick {
                id: method.id.clone(),
                label: method.label.clone(),
                hint: String::new(),
            })
            .collect::<Vec<_>>();
        method = select("Login method", &methods).await?.id;
    }
    let start = client
        .do_method(
            methods::PROVIDER_LOGIN_START,
            &protocol::LoginStartParams {
                provider: chosen.id,
                method,
            },
        )
        .await?;
    if start.method == oauth::METHOD_API_KEY {
        println!(
            "\n{}\n\nCreate a key for your plan at:\n\n    {}\n",
            start.instructions, start.url
        );
        let key = read_line("Paste the key: ", true).await?;
        client
            .do_method(
                methods::PROVIDER_LOGIN_KEY,
                &protocol::LoginKeyParams {
                    id: start.id.clone(),
                    key,
                },
            )
            .await?;
    } else {
        if !start.code.is_empty() {
            println!(
                "\nOpen this URL on any device:\n\n    {}\n\nand enter the code:\n\n    {}\n\n{}",
                start.url, start.code, start.instructions
            );
        } else {
            println!(
                "\nOpening your browser to sign in. If it does not open, visit:\n\n    {}\n\n{}",
                start.url, start.instructions
            );
        }
        open_browser(&start.url);
        println!("Waiting for you to finish signing in… (ctrl+c to cancel)");
    }
    let info = tokio::time::timeout(
        Duration::from_secs(15 * 60),
        client.do_method(
            methods::PROVIDER_LOGIN_WAIT,
            &protocol::LoginWaitParams { id: start.id },
        ),
    )
    .await??;
    if start.method == oauth::METHOD_API_KEY {
        println!("✓ {} connected", info.name);
    } else {
        println!("✓ {} connected ({})", info.name, info.account);
    }
    Ok(())
}
pub async fn input() -> Result<Vec<u8>> {
    let (send, receive) = tokio::sync::oneshot::channel();
    std::thread::spawn(move || {
        let mut input = vec![];
        let result = std::io::stdin()
            .take(4097)
            .read_to_end(&mut input)
            .map(|_| input);
        let _ = send.send(result);
    });
    Ok(receive.await??)
}
