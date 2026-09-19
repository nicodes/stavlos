package protocol

import "time"

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
	DaemonStatus        = Method[None, DaemonStatusResult]{MDaemonStatus}
	DaemonShutdown      = Method[None, None]{MDaemonShutdown}
	DiscordStatusMethod = Method[None, DiscordStatus]{MDiscordStatus}
	DiscordConnect      = Method[None, DiscordStatus]{MDiscordConnect}
	DiscordDisconnect   = Method[None, DiscordStatus]{MDiscordDisconnect}
	WebStatusMethod     = Method[None, WebStatus]{MWebStatus}
	WebEnable           = Method[None, WebStatus]{MWebEnable}
	WebDisable          = Method[None, WebStatus]{MWebDisable}
	WebOpen             = Method[None, WebStatus]{MWebOpen}
	Attach              = Method[AttachParams, AttachResult]{MAttach}

	ChannelList      = Method[ChannelListParams, ChannelListResult]{MChannelList}
	ChannelCreate    = Method[ChannelCreateParams, ChannelInfo]{MChannelCreate}
	ChannelResume    = Method[ChannelRef, ChannelInfo]{MChannelResume}
	ChannelArchive   = Method[ChannelRef, None]{MChannelArchive}
	ChannelRename    = Method[ChannelRenameParams, None]{MChannelRename}
	ChannelSetModel  = Method[ChannelSetModelParams, None]{MChannelSetModel}
	ChannelSetMode   = Method[ChannelSetModeParams, None]{MChannelSetMode}
	ChannelSetRecap  = Method[ChannelSetRecapParams, None]{MChannelSetRecap}
	ChannelPost      = Method[ChannelPostParams, ChannelPostResult]{MChannelPost}
	ChannelAddDir    = Method[ChannelDirParams, None]{MChannelAddDir}
	ChannelRemoveDir = Method[ChannelDirParams, None]{MChannelRemoveDir}
	ChannelSetDir    = Method[ChannelDirParams, ChannelInfo]{MChannelSetDir}

	AgentTree       = Method[AgentTreeParams, AgentTreeResult]{MAgentTree}
	AgentSend       = Method[AgentSendParams, None]{MAgentSend}
	AgentSpawn      = Method[AgentSpawnParams, AgentSpawnResult]{MAgentSpawn}
	AgentSetModel   = Method[AgentSetModelParams, None]{MAgentSetModel}
	AgentSetRole    = Method[AgentSetRoleParams, None]{MAgentSetRole}
	AgentSetVariant = Method[AgentSetVariantParams, None]{MAgentSetVariant}
	AgentCompact    = Method[AgentCompactParams, AgentCompactResult]{MAgentCompact}
	Variants        = Method[VariantsParams, VariantsResult]{MVariants}

	SheetList = Method[ChannelRef, SheetListResult]{MSheetList}

	PromptList  = Method[PromptListParams, PromptListResult]{MPromptList}
	PromptClaim = Method[PromptClaimParams, None]{MPromptClaim}
	PromptReply = Method[PromptReplyParams, None]{MPromptReply}

	TrustStatus = Method[TrustStatusParams, TrustStatusResult]{MTrustStatus}
	TrustReply  = Method[TrustReplyParams, None]{MTrustReply}

	ProviderList       = Method[None, ProviderListResult]{MProviderList}
	ProviderLoginStart = Method[LoginStartParams, LoginStartResult]{MProviderLoginStart}
	ProviderLoginWait  = Method[LoginWaitParams, ProviderInfo]{MProviderLoginWait}
	ProviderLoginKey   = Method[LoginKeyParams, None]{MProviderLoginKey}
	ProviderDisconnect = Method[ProviderRef, None]{MProviderDisconnect}
	ModelList          = Method[ModelListParams, ModelListResult]{MModelList}

	UsageSeries = Method[UsageSeriesParams, UsageSeriesResult]{MUsageSeries}
	PlanUsage   = Method[None, PlanUsageResult]{MPlanUsage}
	CacheUsage  = Method[CacheUsageParams, CacheUsageResult]{MCacheUsage}
	PlanSeries  = Method[PlanSeriesParams, PlanSeriesResult]{MPlanSeries}

	Subscribe   = Method[SubscribeParams, SubscribeResult]{MSubscribe}
	Unsubscribe = Method[SubscribeParams, None]{MUnsubscribe}
	Reconcile   = Method[ChannelRef, ReconcileResult]{MReconcile}
	Presets     = Method[PresetsParams, PresetsResult]{MPresets}
)

// UsageSeriesParams picks whose usage to chart: every channel's (no
// Channel), a channel's, or one of its agents'; from From (the first model
// call when zero) to To (now when zero), in Buckets equal spans (1–1000).
type UsageSeriesParams struct {
	Channel string    `json:"channel,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	From    time.Time `json:"from,omitzero"`
	To      time.Time `json:"to,omitzero"`
	Buckets int       `json:"buckets"`
}

// UsageSeriesResult is input + output tokens and cost per bucket over
// [From, To).
type UsageSeriesResult struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Tokens []int     `json:"tokens"`
	Cost   []float64 `json:"cost_usd"`
}

// PlanUsageResult is the plan usage of every signed-in subscription that
// has reported one (docs/plan-usage.md).
type PlanUsageResult struct {
	Plans []PlanUsageInfo `json:"plans"`
}

// PlanUsageInfo is one subscription's usage windows as its latest model
// call's response reported them, and when that was.
type PlanUsageInfo struct {
	Provider string            `json:"provider"`       // "openai"
	Name     string            `json:"name"`           // "ChatGPT"
	Plan     string            `json:"plan,omitempty"` // "pro", where the provider names it
	Windows  []UsageWindowInfo `json:"windows"`        // shortest first
	Observed time.Time         `json:"observed"`
}

// UsageWindowInfo is one rolling limit: the percent used, its length in
// minutes (0 when unknown) and when it resets (zero when unknown).
type UsageWindowInfo struct {
	UsedPercent float64   `json:"used_percent"`
	Minutes     int       `json:"minutes,omitempty"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// CacheUsageParams asks how much of the model calls of the last Minutes
// (60 when zero) the providers served from their prompt caches.
type CacheUsageParams struct {
	Minutes int `json:"minutes,omitempty"`
}

// CacheUsageResult is what those calls carried: tokens charged as new
// input, and tokens served from the cache.
type CacheUsageResult struct {
	Fresh  int64 `json:"fresh"`
	Cached int64 `json:"cached"`
}

// PlanUsagePoint is a plan's most used window as one reading saw it.
type PlanUsagePoint struct {
	At          time.Time `json:"at"`
	UsedPercent float64   `json:"used_percent"`
}

// PlanSeriesParams asks for a provider's plan usage over [From, To) in
// Buckets equal spans (1–1000); From zero starts at the first reading.
type PlanSeriesParams struct {
	Provider string    `json:"provider"`
	From     time.Time `json:"from,omitzero"`
	To       time.Time `json:"to,omitzero"`
	Buckets  int       `json:"buckets"`
}

// PlanSeriesResult is the percent used per bucket: the highest reading in
// each, an empty bucket carrying the last known one forward.
type PlanSeriesResult struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Percent []float64 `json:"used_percent"`
}
