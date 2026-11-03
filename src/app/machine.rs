//! Finite machine commands use the same typed owner methods as terminal actions.
use super::*;
use serde_json::Value;
impl App {
    pub async fn run_json(&self, arguments: &[String]) -> Result<Value> {
        tokio::select! {
            result = self.machine_command(arguments) => result,
            _ = self.cancel.cancelled() => bail!("command canceled"),
        }
    }
    async fn machine_command(&self, arguments: &[String]) -> Result<Value> {
        let (command, args) = command(arguments);
        match command {
            "version" | "--version" => {
                ensure!(args.is_empty(), "version takes no arguments");
                Ok(
                    json!({"version":option_env!("STAVLOS_BUILD_VERSION").unwrap_or("devel"),"commit":option_env!("STAVLOS_BUILD_COMMIT").unwrap_or(""),"build":buildid::id()}),
                )
            }
            "help" | "--help" | "-h" => {
                ensure!(args.is_empty(), "help takes no arguments");
                Ok(json!({"help":USAGE}))
            }
            "channels" => {
                ensure!(args.is_empty(), "usage: stavlos --json channels");
                Ok(serde_json::to_value(
                    self.connect(false)
                        .await?
                        .do_method(
                            methods::CHANNEL_LIST,
                            &protocol::ChannelListParams {
                                include_archived: true,
                                ..Default::default()
                            },
                        )
                        .await?,
                )?)
            }
            "status" => {
                ensure!(args.is_empty(), "usage: stavlos --json status");
                Ok(serde_json::to_value(
                    self.connect(false)
                        .await?
                        .do_method(methods::DAEMON_STATUS, &protocol::None::default())
                        .await?,
                )?)
            }
            "tree" => {
                ensure!(args.len() == 1, "usage: stavlos --json tree <channel>");
                Ok(serde_json::to_value(
                    self.connect(false)
                        .await?
                        .do_method(
                            methods::AGENT_TREE,
                            &protocol::AgentTreeParams {
                                channel: args[0].clone(),
                            },
                        )
                        .await?,
                )?)
            }
            "send" | "steer" | "cancel" | "kill" => {
                ensure!(
                    !args.is_empty(),
                    "usage: stavlos --json {command} <agent> [text]"
                );
                self.connect(false)
                    .await?
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
                Ok(json!({"agent":args[0],"command":command,"accepted":true}))
            }
            "new"
                if value(
                    &flags(args, &["model", "root", "dir"], &["no-tui"])?,
                    "no-tui",
                ) == "true" =>
            {
                let (_, channel) = self.prepare_start(args, true).await?;
                Ok(serde_json::to_value(channel)?)
            }
            "init" => Ok(json!({"path":self.init_config(args)?,"created":true})),
            "auth" | "provider" | "connect" => {
                let sub = args.first().map(String::as_str).unwrap_or("list");
                ensure!(
                    matches!(sub, "list" | "ls") && args.len() <= 1
                        || matches!(sub, "logout" | "remove" | "rm") && args.len() == 2,
                    "--json supports auth list or auth logout <provider>; login and credential selection require interaction"
                );
                let client = self.connect(true).await?;
                let list = client
                    .do_method(methods::PROVIDER_LIST, &protocol::None::default())
                    .await?;
                if matches!(sub, "list" | "ls") {
                    return Ok(serde_json::to_value(list)?);
                }
                let provider = list
                    .providers
                    .iter()
                    .find(|p| {
                        p.id.eq_ignore_ascii_case(&args[1]) || p.name.eq_ignore_ascii_case(&args[1])
                    })
                    .ok_or_else(|| anyhow::anyhow!("no stored credential for {:?}", args[1]))?;
                client
                    .do_method(
                        methods::PROVIDER_DISCONNECT,
                        &protocol::ProviderRef {
                            provider: provider.id.clone(),
                        },
                    )
                    .await?;
                Ok(json!({"provider":provider.id,"connected":false}))
            }
            "discord" => {
                ensure!(
                    args.len() <= 1,
                    "usage: stavlos --json discord [status|connect|disconnect]"
                );
                let method = match args.first().map(String::as_str).unwrap_or("status") {
                    "status" => methods::DISCORD_STATUS_METHOD,
                    "connect" => methods::DISCORD_CONNECT,
                    "disconnect" => methods::DISCORD_DISCONNECT,
                    _ => bail!("usage: stavlos --json discord [status|connect|disconnect]"),
                };
                Ok(serde_json::to_value(
                    self.connect(true)
                        .await?
                        .do_method(method, &protocol::None::default())
                        .await?,
                )?)
            }
            "web" => {
                ensure!(args.len() <= 1, "usage: stavlos --json web [status|on|off]");
                let method = match args.first().map(String::as_str).unwrap_or("status") {
                    "status" => methods::WEB_STATUS_METHOD,
                    "on" => methods::WEB_ENABLE,
                    "off" => methods::WEB_DISABLE,
                    _ => bail!(
                        "--json supports web status, on or off; use web open --print for a sign-in link"
                    ),
                };
                let status = self
                    .connect(true)
                    .await?
                    .do_method(method, &protocol::None::default())
                    .await?;
                Ok(json!({"enabled":status.enabled,"url":status.url,"error":status.error}))
            }
            "" | "new" | "open" | "trust" | "daemon" | "operator" => bail!(
                "--json requires a finite command; terminal, trust, owner and operator protocols use their native interface (new supports --no-tui)"
            ),
            _ => bail!("unknown command {command:?}\n{USAGE}"),
        }
    }
}
