//! CLI actions use the same owner socket as the terminal and browser clients.
mod auth;
pub use auth::open_browser;
pub mod daemon;
pub mod daemonctl;
use crate::{
    buildid,
    client::Client,
    config, navigation,
    paths::Paths,
    protocol::{self, methods},
    textsafe,
    tools::ServiceFuture,
};
use anyhow::{Result, bail, ensure};
use serde_json::json;
use std::{
    collections::BTreeMap,
    io::Write,
    os::unix::fs::{DirBuilderExt, OpenOptionsExt},
    path::PathBuf,
    sync::Arc,
};
use tokio_util::sync::CancellationToken;
pub trait Terminal: Send + Sync {
    fn run(&self, client: Arc<Client>, channel: String) -> ServiceFuture<'_, ()>;
}
pub struct App {
    pub paths: Paths,
    pub home: PathBuf,
    pub terminal: Arc<dyn Terminal>,
    pub cancel: CancellationToken,
}
pub const USAGE: &str = concat!(
    "usage:\n",
    "  stavlos                                             last viewed channel + global navigation\n",
    "  stavlos new [--model p/m] [--root archetype] [--dir d]   new channel (directory defaults to cwd)\n",
    "  stavlos --dir d [--model p/m] [--root archetype]       new channel with an explicit directory\n",
    "  stavlos open <#name|id>                               a channel by name + TUI\n",
    "  stavlos channels | status | tree <channel>\n",
    "  stavlos discord [status|connect|disconnect]           manage the daemon's Discord integration\n",
    "  stavlos web [status|on|off|open] [--print]            the browser UI on 127.0.0.1:4999 (--print shows the sign-in link instead of opening it)\n",
    "  stavlos send|steer|cancel|kill <agent> [text]\n",
    "  stavlos auth login [provider] | auth list | auth logout [provider]\n",
    "  stavlos trust [dir] | init [--model p/m] | daemon\n",
    "  stavlos operator                       send a finite operator grant from stdin\n",
    "  stavlos version | --version | help\n"
);
pub fn command(args: &[String]) -> (&str, &[String]) {
    match args.split_first() {
        None => ("", args),
        Some((first, rest))
            if !first.starts_with('-')
                || matches!(first.as_str(), "--help" | "-h" | "--version") =>
        {
            (first, rest)
        }
        Some(_) => ("", args),
    }
}
pub fn flags(
    args: &[String],
    names: &[&str],
    boolean: &[&str],
) -> Result<BTreeMap<String, String>> {
    let mut result = BTreeMap::new();
    let mut index = 0;
    while index < args.len() {
        let arg = &args[index];
        if arg == "--" || !arg.starts_with('-') || arg == "-" {
            break;
        }
        let arg = arg.strip_prefix("--").unwrap_or(&arg[1..]);
        let (name, value) = arg
            .split_once('=')
            .map(|(k, v)| (k, Some(v)))
            .unwrap_or((arg, None));
        ensure!(
            names.contains(&name) || boolean.contains(&name),
            "flag provided but not defined: -{name}"
        );
        let value = if boolean.contains(&name) {
            let value = value.unwrap_or("true");
            ensure!(
                matches!(
                    value,
                    "1" | "t"
                        | "T"
                        | "TRUE"
                        | "true"
                        | "True"
                        | "0"
                        | "f"
                        | "F"
                        | "FALSE"
                        | "false"
                        | "False"
                ),
                "invalid value {value:?} for flag -{name}"
            );
            if matches!(value, "1" | "t" | "T" | "TRUE" | "true" | "True") {
                "true"
            } else {
                "false"
            }
        } else {
            match value {
                Some(value) => value,
                None => {
                    index += 1;
                    args.get(index)
                        .map(String::as_str)
                        .ok_or_else(|| anyhow::anyhow!("flag needs an argument: -{name}"))?
                }
            }
        };
        result.insert(name.into(), value.into());
        index += 1;
    }
    Ok(result)
}
fn value<'a>(flags: &'a BTreeMap<String, String>, name: &str) -> &'a str {
    flags.get(name).map(String::as_str).unwrap_or("")
}
impl App {
    async fn connect(&self, autostart: bool) -> Result<Arc<Client>> {
        daemonctl::connect(&self.paths, autostart, &self.cancel).await
    }
    pub async fn run(&self, args: &[String]) -> Result<()> {
        let (command, args) = command(args);
        match command {
            "" | "new" => self.start(args, command == "new").await,
            "open" => {
                if args.is_empty() {
                    return self.start(&[], false).await;
                }
                let client = self.connect(true).await?;
                let wanted = args[0].strip_prefix('#').unwrap_or(&args[0]);
                let channels = client
                    .do_method(
                        methods::CHANNEL_LIST,
                        &protocol::ChannelListParams::default(),
                    )
                    .await?;
                let channel = channels
                    .channels
                    .iter()
                    .find(|channel| channel.id == wanted || channel.name == wanted)
                    .ok_or_else(|| {
                        anyhow::anyhow!("no channel #{wanted} (stavlos channels lists them)")
                    })?;
                let channel = client
                    .do_method(
                        methods::CHANNEL_RESUME,
                        &protocol::ChannelRef {
                            channel: channel.id.clone(),
                        },
                    )
                    .await?;
                self.terminal.run(client, channel.id).await
            }
            "channels" => {
                let client = self.connect(false).await?;
                let channels = client
                    .do_method(
                        methods::CHANNEL_LIST,
                        &protocol::ChannelListParams {
                            include_archived: true,
                            ..Default::default()
                        },
                    )
                    .await?;
                for channel in channels.channels.iter() {
                    println!(
                        "#{:24} {}  {:40} {:30} live={} cost=${:.4} seq={}{}",
                        channel.name,
                        channel.id,
                        channel.dir,
                        channel.model,
                        channel.live_agents,
                        channel.cost_usd,
                        channel.seq,
                        if channel.archived { " (archived)" } else { "" }
                    );
                }
                Ok(())
            }
            "status" => {
                let client = self.connect(false).await?;
                let status = client
                    .do_method(methods::DAEMON_STATUS, &protocol::None::default())
                    .await?;
                println!("{}", serde_json::to_string_pretty(&status)?);
                Ok(())
            }
            "tree" => {
                ensure!(!args.is_empty(), "usage: stavlos tree <channel>");
                let client = self.connect(false).await?;
                let tree = client
                    .do_method(
                        methods::AGENT_TREE,
                        &protocol::AgentTreeParams {
                            channel: args[0].clone(),
                        },
                    )
                    .await?;
                for agent in tree.agents.iter() {
                    println!(
                        "{}{}  {} ({}) {} turn={} ${:.4} {}",
                        "  ".repeat(agent.depth.clamp(0, 128) as usize),
                        agent.id,
                        agent.name,
                        agent.role,
                        agent.state,
                        agent.turn,
                        agent.cost_usd,
                        agent.model
                    );
                }
                Ok(())
            }
            "send" | "steer" | "cancel" | "kill" => {
                ensure!(!args.is_empty(), "usage: stavlos {command} <agent> [text]");
                let client = self.connect(false).await?;
                client
                    .do_method(
                        methods::AGENT_SEND,
                        &protocol::AgentSendParams {
                            agent: args[0].clone(),
                            kind: if command == "send" {
                                protocol::KIND_PROMPT
                            } else {
                                command
                            }
                            .into(),
                            text: args[1..].join(" "),
                        },
                    )
                    .await?;
                Ok(())
            }
            "discord" => {
                ensure!(
                    args.len() <= 1,
                    "usage: stavlos discord [status|connect|disconnect]"
                );
                let method = match args.first().map(String::as_str).unwrap_or("status") {
                    "status" => methods::DISCORD_STATUS_METHOD,
                    "connect" => methods::DISCORD_CONNECT,
                    "disconnect" => methods::DISCORD_DISCONNECT,
                    _ => bail!("usage: stavlos discord [status|connect|disconnect]"),
                };
                let client = self.connect(true).await?;
                println!(
                    "{}",
                    serde_json::to_string_pretty(
                        &client.do_method(method, &protocol::None::default()).await?
                    )?
                );
                Ok(())
            }
            "web" => self.web(args).await,
            "init" => self.init(args),
            "trust" => self.trust(args).await,
            "auth" | "provider" | "connect" => auth::run(self.connect(true).await?, args).await,
            "operator" => {
                ensure!(
                    args.is_empty(),
                    "operator reads one fixed grant request from standard input"
                );
                let input = auth::input().await?;
                let raw = serde_json::from_slice::<Box<serde_json::value::RawValue>>(&input)?;
                let output =
                    crate::tools::operator::run(Some(&raw), self.cancel.child_token()).await;
                ensure!(!output.is_error, "{}", output.output);
                println!("{}", output.output);
                Ok(())
            }
            "plugin" => bail!(
                "plugins are a roadmap item (PRD §11); stavlos serves the ChatGPT (openai), Grok (xai), Z.ai Coding Plan (zai) and Kimi For Coding (kimi) subscriptions: see `stavlos auth login`"
            ),
            "version" | "--version" => {
                print!("{}", buildid::version());
                Ok(())
            }
            "help" | "-h" | "--help" => {
                print!("{USAGE}");
                Ok(())
            }
            "daemon" => {
                daemon::run(
                    self.paths.clone(),
                    self.home.clone(),
                    args,
                    self.cancel.clone(),
                )
                .await
            }
            _ => bail!("unknown command {command:?}\n{USAGE}"),
        }
    }
    async fn start(&self, args: &[String], fresh: bool) -> Result<()> {
        let flags = flags(args, &["model", "root", "dir"], &["no-tui"])?;
        let client = self.connect(true).await?;
        let current = std::env::current_dir()?;
        let dir = if value(&flags, "dir").is_empty() {
            current
        } else {
            config::effective::working_directory(
                &current,
                value(&flags, "dir"),
                &self.home,
                &|name| std::env::var(name).ok(),
            )?
        };
        let channels = client
            .do_method(
                methods::CHANNEL_LIST,
                &protocol::ChannelListParams::default(),
            )
            .await?;
        let last = navigation::resume(&channels.channels, &navigation::last(&self.paths.data));
        let channel = if !last.is_empty()
            && !fresh
            && ["dir", "model", "root"]
                .iter()
                .all(|name| value(&flags, name).is_empty())
        {
            client
                .do_method(
                    methods::CHANNEL_RESUME,
                    &protocol::ChannelRef { channel: last },
                )
                .await?
        } else {
            client
                .do_method(
                    methods::CHANNEL_CREATE,
                    &protocol::ChannelCreateParams {
                        dir: dir.to_string_lossy().into_owned(),
                        model: value(&flags, "model").into(),
                        root_agent: value(&flags, "root").into(),
                        ..Default::default()
                    },
                )
                .await?
        };
        if value(&flags, "no-tui") == "true" {
            println!("{}", channel.id);
            return Ok(());
        }
        self.terminal.run(client, channel.id).await
    }
    async fn web(&self, args: &[String]) -> Result<()> {
        let mut action = "status";
        let mut print = false;
        for arg in args {
            match arg.as_str() {
                "status" | "on" | "off" | "open" => action = arg,
                "--print" => print = true,
                _ => bail!("usage: stavlos web [status|on|off|open] [--print]"),
            }
        }
        let method = match action {
            "on" => methods::WEB_ENABLE,
            "off" => methods::WEB_DISABLE,
            "open" => methods::WEB_OPEN,
            _ => methods::WEB_STATUS_METHOD,
        };
        let client = self.connect(true).await?;
        let status = client.do_method(method, &protocol::None::default()).await?;
        if !status.enabled {
            println!("web UI off");
        } else if !status.open_url.is_empty() && print {
            println!("{}", status.open_url);
        } else if !status.open_url.is_empty() {
            auth::open_browser(&status.open_url);
            println!("web UI on {} (opened in your browser)", status.url);
        } else {
            println!("web UI on {}", status.url);
        }
        Ok(())
    }
    async fn trust(&self, args: &[String]) -> Result<()> {
        let client = self.connect(true).await?;
        let current = std::env::current_dir()?;
        let dir = args.first().map(|dir| current.join(dir)).unwrap_or(current);
        let dir = crate::pathx::clean(&dir).to_string_lossy().into_owned();
        let status = client
            .do_method(
                methods::TRUST_STATUS,
                &protocol::TrustStatusParams { dir: dir.clone() },
            )
            .await?;
        if !status.pending {
            println!("nothing pending for {dir}");
            return Ok(());
        }
        println!(
            "Project configuration in {} is not yet trusted. It can start MCP servers, tighten policy, add roles and skills, and instruct agents.\nFiles:",
            textsafe::visible(&dir)
        );
        for file in status.files.iter() {
            println!("   {}", textsafe::visible(file));
        }
        let answer = auth::read_line("Trust it? [y/N] ", false).await?;
        let trust = answer.to_lowercase().starts_with('y');
        client
            .do_method(
                methods::TRUST_REPLY,
                &protocol::TrustReplyParams {
                    dir,
                    hash: status.hash.clone(),
                    trust,
                },
            )
            .await?;
        if trust {
            println!("trusted {}", status.hash);
        }
        Ok(())
    }
    fn init(&self, args: &[String]) -> Result<()> {
        let flags = flags(args, &["model"], &[])?;
        let path = self.paths.config.join("stavlos.json");
        ensure!(!path.exists(), "{} already exists", path.display());
        for sub in ["agents", "skills"] {
            std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o755)
                .create(self.paths.config.join(sub))?;
        }
        let mut starter = config::effective::defaults();
        starter.model = value(&flags, "model").into();
        starter.policy.insert(
            "shell".into(),
            json!({"*":"ask","git push*":"ask","rm -rf*":"deny"}),
        );
        let mut raw = serde_json::to_vec_pretty(&starter)?;
        raw.push(b'\n');
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&path)?;
        file.write_all(&raw)?;
        file.sync_all()?;
        println!("wrote {}", path.display());
        println!(
            "next:\n  - run stavlos in a project directory, then /providers to sign in and /models to pick a model (or run stavlos auth login now)\n  - add agent definitions as agents/<name>.md next to stavlos.json (see README.md)\n  - directories every channel may work in besides its own: \"dirs\": [\"~/Work/shared\"]\n  - for search on your own quota, add \"search\": {{\"provider\": \"brave\", \"apiKey\": \"${{env:BRAVE_API_KEY}}\"}}\n  - commands run without credential-looking variables; list what a build needs under \"env\": {{\"pass\": [\"GITHUB_TOKEN\"]}}"
        );
        Ok(())
    }
}
