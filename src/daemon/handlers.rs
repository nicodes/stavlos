use super::{
    logins::provider_info,
    owner::Daemon,
    server::{Connection, Dispatch, Handler},
};
use crate::{
    agent::{routing::Routing, runtime::Spawn, security},
    event,
    protocol::{self, methods::Method},
    wire::Time,
};
use anyhow::{Result, ensure};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;
fn params<P: DeserializeOwned + Default, R>(
    request: &protocol::Request,
    _method: Method<P, R>,
) -> Result<P> {
    Ok(if let Some(raw) = &request.params {
        serde_json::from_str(raw.get()).map_err(|error| {
            protocol::Error::new(protocol::ERR_INVALID_PARAMS, error.to_string())
        })?
    } else {
        P::default()
    })
}
fn encode<P, R: Serialize>(_method: Method<P, R>, result: Result<R>) -> Result<Value> {
    Ok(serde_json::to_value(result?)
        .map_err(|error| protocol::Error::new(protocol::ERR_INTERNAL, error.to_string()))?)
}
fn error(error: anyhow::Error) -> protocol::Error {
    if let Some(error) = error.downcast_ref::<protocol::Error>() {
        return error.clone();
    }
    protocol::Error::new(protocol::ERR_INVALID_PARAMS, format!("{error:#}"))
}
macro_rules! route {
    ($request:ident,$method:ident,$params:ident,$body:block)=>{{
        let method=protocol::methods::$method;
        let $params=params(&$request,method)?;
        encode(method,(async $body).await)
    }};
}
impl Handler for Daemon {
    fn dispatch<'a>(&'a self, c: &'a Connection, request: protocol::Request) -> Dispatch<'a> {
        Box::pin(async move { self.handle(c, request).await.map_err(error) })
    }
    fn disconnected(&self, client: &str) {
        self.prompts.release_client(client);
    }
}
impl Daemon {
    async fn handle(&self, c: &Connection, request: protocol::Request) -> Result<Value> {
        let cancel = &c.cancel;
        let source = format!("human:{}", c.client.identity().0);
        match request.method.as_str() {
            protocol::MATTACH => route!(request, ATTACH, p, {
                c.client
                    .attach(&p.client, &p.tier, c.scope == super::server::Scope::Web);
                Ok(protocol::AttachResult {
                    client_id: c.client.id.clone(),
                    version: protocol::VERSION,
                })
            }),
            protocol::MDAEMON_STATUS => route!(request, DAEMON_STATUS, _p, { Ok(self.status()) }),
            protocol::MDAEMON_SHUTDOWN => route!(request, DAEMON_SHUTDOWN, _p, {
                let cancel = self.cancel.clone();
                tokio::spawn(async move {
                    tokio::task::yield_now().await;
                    cancel.cancel();
                });
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_LIST => route!(request, CHANNEL_LIST, p, {
                Ok(protocol::ChannelListResult {
                    channels: self.channel_list(&p.dir, p.include_archived)?.into(),
                })
            }),
            protocol::MCHANNEL_CREATE => route!(request, CHANNEL_CREATE, p, {
                Ok(self
                    .create(&p.dir, &p.model, &p.root_agent, &p.name, cancel)?
                    .info())
            }),
            protocol::MCHANNEL_RESUME => route!(request, CHANNEL_RESUME, p, {
                let channel = self.channel(&p.channel)?;
                self.maybe_trust_prompt(&channel)?;
                Ok(channel.info())
            }),
            protocol::MCHANNEL_ARCHIVE => route!(request, CHANNEL_ARCHIVE, p, {
                self.archive(&p.channel, cancel).await?;
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_RENAME => route!(request, CHANNEL_RENAME, p, {
                self.rename(&p.channel, &p.name, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_SET_MODEL => route!(request, CHANNEL_SET_MODEL, p, {
                let channel = self.channel(&p.channel)?;
                channel.set_model(&p.model, cancel)?;
                self.remember_model(&channel, &p.model);
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_POST => route!(request, CHANNEL_POST, p, {
                let channel = self.channel(&p.channel)?;
                Ok(protocol::ChannelPostResult {
                    to: self.post(&channel, &p.text, &source, cancel)?.into(),
                })
            }),
            protocol::MCHANNEL_SET_RECAP => route!(request, CHANNEL_SET_RECAP, p, {
                let channel = self.channel(&p.channel)?;
                Routing {
                    core: channel.core.clone(),
                }
                .set_recap(p.minutes, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_SET_MODE => route!(request, CHANNEL_SET_MODE, p, {
                let channel = self.channel(&p.channel)?;
                channel.set_mode(&p.mode, cancel)?;
                for (value, verb) in [
                    ("allow", crate::policy::Verb::Allow),
                    ("deny", crate::policy::Verb::Deny),
                ] {
                    self.prompts.answer_where(
                        &p.channel,
                        protocol::PROMPT_PERMISSION,
                        value,
                        &p.mode,
                        |prompt| {
                            security::mode_verdict(
                                &p.mode,
                                prompt.sticky,
                                prompt.egress,
                                !prompt.dir.is_empty(),
                            ) == verb
                        },
                    );
                }
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_ADD_DIR => route!(request, CHANNEL_ADD_DIR, p, {
                self.channel(&p.channel)?
                    .add_directory_from_owner(&p.dir, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_REMOVE_DIR => route!(request, CHANNEL_REMOVE_DIR, p, {
                self.channel(&p.channel)?.remove_directory(&p.dir, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MCHANNEL_SET_DIR => route!(request, CHANNEL_SET_DIR, p, {
                self.set_directory(&p.channel, &p.dir, cancel).await
            }),
            protocol::MSHEET_LIST => route!(request, SHEET_LIST, p, {
                let channel = self.channel(&p.channel)?;
                Ok(protocol::SheetListResult {
                    sheets: crate::agent::sheets::SheetService {
                        core: channel.core.clone(),
                        agent: String::new(),
                        directory: crate::agent::sheets::directory(&self.paths, &p.channel),
                    }
                    .infos()
                    .into(),
                })
            }),
            protocol::MAGENT_TREE => route!(request, AGENT_TREE, p, {
                Ok(protocol::AgentTreeResult {
                    agents: self.channel_tree(self.channel(&p.channel)?.as_ref()).into(),
                })
            }),
            protocol::MAGENT_SEND => route!(request, AGENT_SEND, p, {
                let (channel, _) = self.agent(&p.agent)?;
                match p.kind.as_str() {
                    "prompt" | "steer" => channel.queue(
                        &p.agent,
                        &p.text,
                        &source,
                        if p.kind == "prompt" {
                            event::INPUT_PROMPT
                        } else {
                            event::INPUT_STEER
                        },
                        cancel,
                    )?,
                    "cancel" => channel.cancel(&p.agent)?,
                    "kill" => channel.kill(&p.agent).await?,
                    _ => anyhow::bail!("unknown envelope kind {:?}", p.kind),
                }
                Ok(protocol::None::default())
            }),
            protocol::MAGENT_SPAWN => route!(request, AGENT_SPAWN, p, {
                let (channel, _) = self.agent(&p.parent)?;
                let actor = channel.spawn(
                    Spawn {
                        parent: p.parent,
                        role: p.role,
                        label: p.name,
                        task: p.task,
                        model: p.model,
                        directories: p.directories.to_vec(),
                    },
                    cancel,
                )?;
                Ok(protocol::AgentSpawnResult {
                    id: actor.id.clone(),
                })
            }),
            protocol::MAGENT_SET_MODEL => route!(request, AGENT_SET_MODEL, p, {
                let (channel, actor) = self.agent(&p.agent)?;
                actor.set_model(&p.model, cancel)?;
                if actor.parent.is_empty()
                    && channel.core.inspect(|state, _| state.model.is_empty())
                {
                    let _ = channel.set_model(&p.model, cancel);
                    self.remember_model(&channel, &p.model);
                }
                Ok(protocol::None::default())
            }),
            protocol::MAGENT_SET_ROLE => route!(request, AGENT_SET_ROLE, p, {
                self.agent(&p.agent)?.1.set_role(&p.role, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MAGENT_SET_VARIANT => route!(request, AGENT_SET_VARIANT, p, {
                self.agent(&p.agent)?.1.set_variant(&p.variant, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MAGENT_COMPACT => route!(request, AGENT_COMPACT, p, {
                Ok(protocol::AgentCompactResult {
                    status: self
                        .agent(&p.agent)?
                        .1
                        .compact(cancel.clone())
                        .await?
                        .into(),
                })
            }),
            protocol::MVARIANTS => route!(request, VARIANTS, p, {
                Ok(protocol::VariantsResult {
                    variants: self.registry.variants(&p.model).into(),
                })
            }),
            protocol::MPROMPT_LIST => route!(request, PROMPT_LIST, p, {
                Ok(protocol::PromptListResult {
                    prompts: self.prompts.pending(&p.channel).into(),
                })
            }),
            protocol::MPROMPT_CLAIM => route!(request, PROMPT_CLAIM, p, {
                self.prompts
                    .claim(&p.id, &c.client.id)
                    .map_err(prompt_error)?;
                Ok(protocol::None::default())
            }),
            protocol::MPROMPT_REPLY => route!(request, PROMPT_REPLY, p, {
                self.prompts
                    .reply(
                        &p.id,
                        &c.client.id,
                        crate::escalation::Answer {
                            value: p.answer,
                            dir: p.dir,
                            reason: p.reason,
                            answers: p.answers.to_vec(),
                            details: p.details.to_vec(),
                            ..Default::default()
                        },
                    )
                    .map_err(prompt_error)?;
                Ok(protocol::None::default())
            }),
            protocol::MTRUST_STATUS => {
                route!(request, TRUST_STATUS, p, { self.trust_status(&p.dir) })
            }
            protocol::MTRUST_REPLY => route!(request, TRUST_REPLY, p, {
                self.trust(&p.dir, &p.hash, p.trust)?;
                Ok(protocol::None::default())
            }),
            protocol::MPROVIDER_LIST => route!(request, PROVIDER_LIST, _p, {
                Ok(protocol::ProviderListResult {
                    auth_path: self.paths.auth_file().to_string_lossy().into_owned(),
                    providers: self
                        .registry
                        .list()
                        .into_iter()
                        .map(provider_info)
                        .collect(),
                })
            }),
            protocol::MPROVIDER_LOGIN_START => route!(request, PROVIDER_LOGIN_START, p, {
                self.login_start(&p.provider, &p.method).await
            }),
            protocol::MPROVIDER_LOGIN_WAIT => route!(request, PROVIDER_LOGIN_WAIT, p, {
                self.login_wait(&p.id, cancel.clone()).await
            }),
            protocol::MPROVIDER_LOGIN_KEY => route!(request, PROVIDER_LOGIN_KEY, p, {
                self.login_key(&p.id, &p.key)?;
                Ok(protocol::None::default())
            }),
            protocol::MPROVIDER_DISCONNECT => route!(request, PROVIDER_DISCONNECT, p, {
                self.registry.disconnect(&p.provider)?;
                Ok(protocol::None::default())
            }),
            protocol::MMODEL_LIST => route!(request, MODEL_LIST, p, {
                Ok(protocol::ModelListResult {
                    models: self
                        .registry
                        .models(&p.provider, p.all)
                        .into_iter()
                        .map(|model| protocol::ModelInfo {
                            id: model.id,
                            provider: model.provider,
                            name: model.name,
                            context: model.info.context_window,
                            input_price: model.info.input_price,
                            output_price: model.info.output_price,
                        })
                        .collect(),
                })
            }),
            protocol::MSUBSCRIBE => route!(request, SUBSCRIBE, p, {
                self.channel(&p.channel)?;
                let mut from = p.from;
                let mut first = 0;
                if p.tail > 0 && from <= 1 {
                    let start = self.log.last_seq(&p.channel)? - p.tail + 1;
                    if start > 1 {
                        from = start;
                        first = start;
                    }
                }
                let seq = c
                    .client
                    .subscribe(&self.log, &p.channel, from, cancel)
                    .await?;
                Ok(protocol::SubscribeResult { seq, first })
            }),
            protocol::MUNSUBSCRIBE => route!(request, UNSUBSCRIBE, p, {
                c.client.unsubscribe(&p.channel);
                Ok(protocol::None::default())
            }),
            protocol::MRECONCILE => route!(request, RECONCILE, p, {
                let channel = self.channel(&p.channel)?;
                let seq = self.log.last_seq(&p.channel)?;
                let mut info = channel.info();
                info.seq = seq;
                Ok(protocol::ReconcileResult {
                    channel: info,
                    agents: self.channel_tree(&channel).into(),
                    prompts: self.prompts.pending("").into(),
                    seq,
                })
            }),
            protocol::MROLES => route!(request, ROLES, p, {
                Ok(protocol::RolesResult {
                    roles: self.channel(&p.channel)?.roles().into(),
                })
            }),
            protocol::MUSAGE_SERIES => route!(request, USAGE_SERIES, p, {
                ensure!(
                    (1..=1000).contains(&p.buckets),
                    "buckets must be 1–1000, not {}",
                    p.buckets
                );
                ensure!(
                    p.agent.is_empty() || !p.channel.is_empty(),
                    "an agent's usage needs its channel"
                );
                ensure!(
                    p.from.is_zero() || p.to.is_zero() || p.from.0 < p.to.0,
                    "from must be before to"
                );
                let usage = self.log.usage(
                    &p.channel,
                    &p.agent,
                    if p.from.is_zero() {
                        None
                    } else {
                        Some(p.from.0.to_utc())
                    },
                    if p.to.is_zero() {
                        None
                    } else {
                        Some(p.to.0.to_utc())
                    },
                    p.buckets as usize,
                )?;
                Ok(protocol::UsageSeriesResult {
                    from: Time(usage.from.fixed_offset()),
                    to: Time(usage.to.fixed_offset()),
                    tokens: usage.tokens.into(),
                    cost_usd: usage.cost.into(),
                })
            }),
            protocol::MPLAN_USAGE => route!(request, PLAN_USAGE, _p, { Ok(self.plan_usage()) }),
            protocol::MPLAN_SERIES => route!(request, PLAN_SERIES, p, {
                self.plan_series(p, cancel).await
            }),
            protocol::MCACHE_USAGE => route!(request, CACHE_USAGE, p, {
                let minutes = if p.minutes == 0 { 60 } else { p.minutes };
                ensure!(
                    (1..=10080).contains(&minutes),
                    "minutes must be 1–10080, not {minutes}"
                );
                let usage = self
                    .log
                    .cache_usage(chrono::Utc::now() - chrono::TimeDelta::minutes(minutes))?;
                let mut providers = usage
                    .into_iter()
                    .map(|(provider, [fresh, cached])| protocol::CacheProviderUsage {
                        provider,
                        fresh,
                        cached,
                    })
                    .collect::<Vec<_>>();
                let [fresh, cached] = self
                    .log
                    .cache_totals(chrono::Utc::now() - chrono::TimeDelta::minutes(minutes))?;
                providers.sort_by(|a, b| {
                    (b.fresh + b.cached)
                        .cmp(&(a.fresh + a.cached))
                        .then(a.provider.cmp(&b.provider))
                });
                Ok(protocol::CacheUsageResult {
                    fresh,
                    cached,
                    providers: providers.into(),
                })
            }),
            protocol::MCOMMAND_LIST => {
                route!(request, COMMAND_LIST, p, { self.commands(&p.channel) })
            }
            protocol::MCOMMAND_RUN => route!(request, COMMAND_RUN, p, {
                self.run_command(p, &source, cancel)?;
                Ok(protocol::None::default())
            }),
            protocol::MCONFIG_LIST => route!(request, CONFIG_LIST, p, { self.config_list(p) }),
            protocol::MCONFIG_READ => route!(request, CONFIG_READ, p, { self.config_read(p) }),
            protocol::MCONFIG_EDIT => route!(request, CONFIG_EDIT, p, { self.config_edit(p) }),
            protocol::MSANDBOX_STATUS => route!(request, SANDBOX_STATUS_METHOD, _p, {
                self.sandbox_status()
            }),
            protocol::MSANDBOX_SET => {
                route!(request, SANDBOX_SET, p, { self.set_sandbox(p.enabled) })
            }
            protocol::MWEB_STATUS
            | protocol::MWEB_ENABLE
            | protocol::MWEB_DISABLE
            | protocol::MWEB_OPEN => {
                // All four share exactly the same typed request and response contract.
                let _: protocol::None = params(&request, protocol::methods::WEB_STATUS_METHOD)?;
                let service =
                    self.web.read().unwrap().clone().ok_or_else(|| {
                        anyhow::anyhow!("the web UI is unavailable in this daemon")
                    })?;
                encode(
                    protocol::methods::WEB_STATUS_METHOD,
                    service.call(&request.method, cancel.clone()).await,
                )
            }
            protocol::MDISCORD_STATUS
            | protocol::MDISCORD_CONNECT
            | protocol::MDISCORD_DISCONNECT => {
                let _: protocol::None = params(&request, protocol::methods::DISCORD_STATUS_METHOD)?;
                let service = self.discord.read().unwrap().clone();
                if let Some(service) = service {
                    encode(
                        protocol::methods::DISCORD_STATUS_METHOD,
                        service.call(&request.method, cancel.clone()).await,
                    )
                } else if request.method == protocol::MDISCORD_STATUS {
                    encode(
                        protocol::methods::DISCORD_STATUS_METHOD,
                        Ok(protocol::DiscordStatus {
                            state: "disconnected".into(),
                            error: "Discord service is unavailable in this daemon".into(),
                            ..Default::default()
                        }),
                    )
                } else {
                    anyhow::bail!("discord service is unavailable in this daemon")
                }
            }
            _ => Err(protocol::Error::new(
                protocol::ERR_METHOD_NOT_FOUND,
                format!("unknown method {}", request.method),
            )
            .into()),
        }
    }
}
fn prompt_error(error: anyhow::Error) -> anyhow::Error {
    // Escalation deliberately exposes no internal state beyond these stable errors.
    let message = error.to_string();
    let conflict = message.contains("claimed")
        || message.contains("already resolved")
        || message.contains("too late");
    protocol::Error::new(
        if conflict {
            protocol::ERR_CONFLICT
        } else {
            protocol::ERR_NOT_FOUND
        },
        message,
    )
    .into()
}
