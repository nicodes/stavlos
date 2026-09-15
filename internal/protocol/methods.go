package protocol

// Method is one request of the protocol: its name, the params it takes (P)
// and the result it returns (R). The client calls a Method and the daemon
// routes one, so a method's name and its shapes are declared once, here,
// and a call with the wrong params or result does not compile.
type Method[P, R any] struct{ Name string }

// None is the params of a method that takes none, and the result of one
// that returns nothing but success.
type None struct{}

// SubscribeResult is the last seq a subscription replayed; live events
// follow from the next.
type SubscribeResult struct {
	Seq int64 `json:"seq"`
}

// The methods.
var (
	DaemonStatus   = Method[None, DaemonStatusResult]{MDaemonStatus}
	DaemonShutdown = Method[None, None]{MDaemonShutdown}
	Attach         = Method[AttachParams, AttachResult]{MAttach}

	ChannelList      = Method[ChannelListParams, ChannelListResult]{MChannelList}
	ChannelCreate    = Method[ChannelCreateParams, ChannelInfo]{MChannelCreate}
	ChannelResume    = Method[ChannelRef, ChannelInfo]{MChannelResume}
	ChannelArchive   = Method[ChannelRef, None]{MChannelArchive}
	ChannelRename    = Method[ChannelRenameParams, None]{MChannelRename}
	ChannelSetModel  = Method[ChannelSetModelParams, None]{MChannelSetModel}
	ChannelSetMode   = Method[ChannelSetModeParams, None]{MChannelSetMode}
	ChannelPost      = Method[ChannelPostParams, ChannelPostResult]{MChannelPost}
	ChannelAddDir    = Method[ChannelDirParams, None]{MChannelAddDir}
	ChannelRemoveDir = Method[ChannelDirParams, None]{MChannelRemoveDir}

	AgentTree       = Method[AgentTreeParams, AgentTreeResult]{MAgentTree}
	AgentSend       = Method[AgentSendParams, None]{MAgentSend}
	AgentSpawn      = Method[AgentSpawnParams, AgentSpawnResult]{MAgentSpawn}
	AgentSetModel   = Method[AgentSetModelParams, None]{MAgentSetModel}
	AgentSetRole    = Method[AgentSetRoleParams, None]{MAgentSetRole}
	AgentSetVariant = Method[AgentSetVariantParams, None]{MAgentSetVariant}
	AgentCompact    = Method[AgentCompactParams, AgentCompactResult]{MAgentCompact}
	Variants        = Method[VariantsParams, VariantsResult]{MVariants}

	PromptList  = Method[PromptListParams, PromptListResult]{MPromptList}
	PromptClaim = Method[PromptClaimParams, None]{MPromptClaim}
	PromptReply = Method[PromptReplyParams, None]{MPromptReply}

	TrustStatus = Method[TrustStatusParams, TrustStatusResult]{MTrustStatus}
	TrustReply  = Method[TrustReplyParams, None]{MTrustReply}

	ProviderList       = Method[None, ProviderListResult]{MProviderList}
	ProviderLoginStart = Method[LoginStartParams, LoginStartResult]{MProviderLoginStart}
	ProviderLoginWait  = Method[LoginWaitParams, ProviderInfo]{MProviderLoginWait}
	ProviderDisconnect = Method[ProviderRef, None]{MProviderDisconnect}
	ModelList          = Method[ModelListParams, ModelListResult]{MModelList}

	Subscribe   = Method[SubscribeParams, SubscribeResult]{MSubscribe}
	Unsubscribe = Method[SubscribeParams, None]{MUnsubscribe}
	Reconcile   = Method[ChannelRef, ReconcileResult]{MReconcile}
	Presets     = Method[PresetsParams, PresetsResult]{MPresets}
)
