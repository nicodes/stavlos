//! Each method fixes both its request and response types.
use super::*;
pub struct Method<P, R> {
    pub name: &'static str,
    marker: std::marker::PhantomData<fn(P) -> R>,
}
impl<P, R> Copy for Method<P, R> {}
impl<P, R> Clone for Method<P, R> {
    fn clone(&self) -> Self {
        *self
    }
}
impl<P, R> Method<P, R> {
    pub const fn new(name: &'static str) -> Self {
        Self {
            name,
            marker: std::marker::PhantomData,
        }
    }
}
pub const COMMAND_LIST: Method<ChannelRef, CommandListResult> = Method::new(MCOMMAND_LIST);
pub const COMMAND_RUN: Method<CommandRunParams, None> = Method::new(MCOMMAND_RUN);
pub const CONFIG_LIST: Method<ConfigScope, ConfigTree> = Method::new(MCONFIG_LIST);
pub const CONFIG_READ: Method<ConfigFileParams, ConfigDocument> = Method::new(MCONFIG_READ);
pub const CONFIG_EDIT: Method<ConfigEditParams, ConfigEditResult> = Method::new(MCONFIG_EDIT);
pub const DAEMON_STATUS: Method<None, DaemonStatusResult> = Method::new(MDAEMON_STATUS);
pub const DAEMON_SHUTDOWN: Method<None, None> = Method::new(MDAEMON_SHUTDOWN);
pub const DISCORD_STATUS_METHOD: Method<None, DiscordStatus> = Method::new(MDISCORD_STATUS);
pub const DISCORD_CONNECT: Method<None, DiscordStatus> = Method::new(MDISCORD_CONNECT);
pub const DISCORD_DISCONNECT: Method<None, DiscordStatus> = Method::new(MDISCORD_DISCONNECT);
pub const WEB_STATUS_METHOD: Method<None, WebStatus> = Method::new(MWEB_STATUS);
pub const WEB_ENABLE: Method<None, WebStatus> = Method::new(MWEB_ENABLE);
pub const SANDBOX_STATUS_METHOD: Method<None, SandboxStatus> = Method::new(MSANDBOX_STATUS);
pub const SANDBOX_SET: Method<SandboxSetParams, SandboxStatus> = Method::new(MSANDBOX_SET);
pub const WEB_DISABLE: Method<None, WebStatus> = Method::new(MWEB_DISABLE);
pub const WEB_OPEN: Method<None, WebStatus> = Method::new(MWEB_OPEN);
pub const ATTACH: Method<AttachParams, AttachResult> = Method::new(MATTACH);
pub const CHANNEL_LIST: Method<ChannelListParams, ChannelListResult> = Method::new(MCHANNEL_LIST);
pub const CHANNEL_CREATE: Method<ChannelCreateParams, ChannelInfo> = Method::new(MCHANNEL_CREATE);
pub const CHANNEL_RESUME: Method<ChannelRef, ChannelInfo> = Method::new(MCHANNEL_RESUME);
pub const CHANNEL_ARCHIVE: Method<ChannelRef, None> = Method::new(MCHANNEL_ARCHIVE);
pub const CHANNEL_RENAME: Method<ChannelRenameParams, None> = Method::new(MCHANNEL_RENAME);
pub const CHANNEL_SET_MODEL: Method<ChannelSetModelParams, None> = Method::new(MCHANNEL_SET_MODEL);
pub const CHANNEL_SET_MODE: Method<ChannelSetModeParams, None> = Method::new(MCHANNEL_SET_MODE);
pub const CHANNEL_SET_RECAP: Method<ChannelSetRecapParams, None> = Method::new(MCHANNEL_SET_RECAP);
pub const CHANNEL_POST: Method<ChannelPostParams, ChannelPostResult> = Method::new(MCHANNEL_POST);
pub const CHANNEL_ADD_DIR: Method<ChannelDirParams, None> = Method::new(MCHANNEL_ADD_DIR);
pub const CHANNEL_REMOVE_DIR: Method<ChannelDirParams, None> = Method::new(MCHANNEL_REMOVE_DIR);
pub const CHANNEL_SET_DIR: Method<ChannelDirParams, ChannelInfo> = Method::new(MCHANNEL_SET_DIR);
pub const AGENT_TREE: Method<AgentTreeParams, AgentTreeResult> = Method::new(MAGENT_TREE);
pub const AGENT_SEND: Method<AgentSendParams, None> = Method::new(MAGENT_SEND);
pub const AGENT_SPAWN: Method<AgentSpawnParams, AgentSpawnResult> = Method::new(MAGENT_SPAWN);
pub const AGENT_SET_MODEL: Method<AgentSetModelParams, None> = Method::new(MAGENT_SET_MODEL);
pub const AGENT_SET_ROLE: Method<AgentSetRoleParams, None> = Method::new(MAGENT_SET_ROLE);
pub const AGENT_SET_VARIANT: Method<AgentSetVariantParams, None> = Method::new(MAGENT_SET_VARIANT);
pub const AGENT_COMPACT: Method<AgentCompactParams, AgentCompactResult> =
    Method::new(MAGENT_COMPACT);
pub const VARIANTS: Method<VariantsParams, VariantsResult> = Method::new(MVARIANTS);
pub const SHEET_LIST: Method<ChannelRef, SheetListResult> = Method::new(MSHEET_LIST);
pub const PROMPT_LIST: Method<PromptListParams, PromptListResult> = Method::new(MPROMPT_LIST);
pub const PROMPT_CLAIM: Method<PromptClaimParams, None> = Method::new(MPROMPT_CLAIM);
pub const PROMPT_REPLY: Method<PromptReplyParams, None> = Method::new(MPROMPT_REPLY);
pub const TRUST_STATUS: Method<TrustStatusParams, TrustStatusResult> = Method::new(MTRUST_STATUS);
pub const TRUST_REPLY: Method<TrustReplyParams, None> = Method::new(MTRUST_REPLY);
pub const PROVIDER_LIST: Method<None, ProviderListResult> = Method::new(MPROVIDER_LIST);
pub const PROVIDER_LOGIN_START: Method<LoginStartParams, LoginStartResult> =
    Method::new(MPROVIDER_LOGIN_START);
pub const PROVIDER_LOGIN_WAIT: Method<LoginWaitParams, ProviderInfo> =
    Method::new(MPROVIDER_LOGIN_WAIT);
pub const PROVIDER_LOGIN_KEY: Method<LoginKeyParams, None> = Method::new(MPROVIDER_LOGIN_KEY);
pub const PROVIDER_DISCONNECT: Method<ProviderRef, None> = Method::new(MPROVIDER_DISCONNECT);
pub const MODEL_LIST: Method<ModelListParams, ModelListResult> = Method::new(MMODEL_LIST);
pub const USAGE_SERIES: Method<UsageSeriesParams, UsageSeriesResult> = Method::new(MUSAGE_SERIES);
pub const PLAN_USAGE: Method<None, PlanUsageResult> = Method::new(MPLAN_USAGE);
pub const CACHE_USAGE: Method<CacheUsageParams, CacheUsageResult> = Method::new(MCACHE_USAGE);
pub const PLAN_SERIES: Method<PlanSeriesParams, PlanSeriesResult> = Method::new(MPLAN_SERIES);
pub const SUBSCRIBE: Method<SubscribeParams, SubscribeResult> = Method::new(MSUBSCRIBE);
pub const UNSUBSCRIBE: Method<SubscribeParams, None> = Method::new(MUNSUBSCRIBE);
pub const RECONCILE: Method<ChannelRef, ReconcileResult> = Method::new(MRECONCILE);
pub const ROLES: Method<RolesParams, RolesResult> = Method::new(MROLES);
