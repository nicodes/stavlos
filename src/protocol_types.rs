//! Typed owner RPC shapes, retaining the existing v1 wire representation.
use super::Error;
use crate::wire::{Slice, Time};
use crate::wire_struct;
use serde_json::value::RawValue;
wire_struct!(AgentInfo {
    directories:"directories":Slice<String> => optional,
    nudges:"nudges":i64 => optional,
    nudge_limit:"nudge_limit":i64 => optional,
    pending_replies:"pending_replies":Slice<crate::event::ReplyRequest> => optional,
    awaiting_replies:"awaiting_replies":Slice<crate::event::ReplyRequest> => optional,
    id:"id":String => required,
    channel:"channel":String => required,
    parent:"parent":String => optional,
    role:"role":String => required,
    name:"name":String => required,
    model:"model":String => required,
    variant:"variant":String => optional,
    depth:"depth":i64 => required,
    state:"state":AgentState => required,
    turn:"turn":i64 => required,
    queued:"queued":i64 => required,
    cost_usd:"cost_usd":f64 => float,
    tokens:"tokens":i64 => required,
    context:"context":i64 => optional,
    context_window:"context_window":i64 => optional,
    last_error:"last_error":String => optional,
    jobs:"jobs":Slice<JobInfo> => optional,
    todos:"todos":Slice<crate::event::TodoItem> => optional,
    mcp:"mcp":Slice<MCPInfo> => optional,
    awaiting:"awaiting":Slice<String> => optional,
    due:"due":Slice<String> => optional,
});
wire_struct!(AgentTreeParams {
    channel:"channel":String => required,
});
wire_struct!(AgentTreeResult {
    agents:"agents":Slice<AgentInfo> => required,
});
wire_struct!(AgentSendParams {
    agent:"agent":String => required,
    kind:"kind":Kind => required,
    text:"text":String => optional,
});
wire_struct!(AgentSpawnParams {
    directories:"directories":Slice<String> => optional,
    parent:"parent":String => required,
    role:"role":String => required,
    name:"name":String => required,
    task:"task":String => required,
    model:"model":String => optional,
});
wire_struct!(AgentSpawnResult {
    id:"id":String => required,
});
wire_struct!(AgentSetModelParams {
    agent:"agent":String => required,
    model:"model":String => required,
});
wire_struct!(AgentSetRoleParams {
    agent:"agent":String => required,
    role:"role":String => required,
});
wire_struct!(AgentCompactParams {
    agent:"agent":String => required,
});
wire_struct!(AgentCompactResult {
    status:"status":String => required,
});
wire_struct!(AgentSetVariantParams {
    agent:"agent":String => required,
    variant:"variant":String => required,
});
wire_struct!(VariantsParams {
    model:"model":String => required,
});
wire_struct!(VariantsResult {
    variants:"variants":Slice<String> => required,
});
wire_struct!(RolesParams {
    channel:"channel":String => required,
});
wire_struct!(RoleInfo {
    name:"name":String => required,
    description:"description":String => required,
    r#type:"type":String => required,
    models:"models":Slice<ModelSpec> => optional,
    spawn:"spawn":Slice<String> => optional,
    color:"color":String => optional,
    max_turns:"max_turns":i64 => optional,
});
wire_struct!(ModelSpec {
    id:"id":String => required,
    variants:"variants":Slice<String> => optional,
});
wire_struct!(RolesResult {
    roles:"roles":Slice<RoleInfo> => required,
});
wire_struct!(ProviderInfo {
    id:"id":String => required,
    name:"name":String => required,
    connected:"connected":bool => required,
    kind:"kind":String => required,
    priority:"priority":i64 => required,
    models:"models":i64 => required,
    label:"label":String => optional,
    account:"account":String => optional,
    methods:"methods":Slice<LoginMethod> => optional,
});
wire_struct!(LoginMethod {
    id:"id":String => required,
    label:"label":String => required,
});
wire_struct!(LoginStartParams {
    provider:"provider":String => required,
    method:"method":String => optional,
});
wire_struct!(ProviderListParams {});
wire_struct!(ProviderListResult {
    auth_path:"auth_path":String => required,
    providers:"providers":Slice<ProviderInfo> => required,
});
wire_struct!(LoginStartResult {
    id:"id":String => required,
    provider:"provider":String => required,
    method:"method":String => required,
    url:"url":String => required,
    code:"code":String => required,
    instructions:"instructions":String => required,
    expires_in:"expires_in":i64 => required,
});
wire_struct!(LoginWaitParams {
    id:"id":String => required,
});
wire_struct!(LoginKeyParams {
    id:"id":String => required,
    key:"key":String => required,
});
wire_struct!(ProviderRef {
    provider:"provider":String => required,
});
wire_struct!(ModelInfo {
    id:"id":String => required,
    provider:"provider":String => required,
    name:"name":String => required,
    context:"context":i64 => optional,
    input_price:"input_price":f64 => float_optional,
    output_price:"output_price":f64 => float_optional,
});
wire_struct!(ModelListParams {
    provider:"provider":String => optional,
    all:"all":bool => optional,
});
wire_struct!(ModelListResult {
    models:"models":Slice<ModelInfo> => required,
});
wire_struct!(EventNotification {
    event:"event":crate::event::Event => required,
});
wire_struct!(StreamNotification {
    channel:"channel":String => required,
    agent:"agent":String => required,
    turn:"turn":i64 => required,
    text:"text":String => optional,
    thinking:"thinking":String => optional,
    tool_name:"tool_name":String => optional,
    reset:"reset":bool => optional,
});
wire_struct!(PromptNotification {
    action:"action":PromptAction => required,
    prompt:"prompt":PromptInfo => required,
});
wire_struct!(DirInfo {
    path:"path":String => required,
    source:"source":String => required,
});
wire_struct!(MCPInfo {
    name:"name":String => required,
    state:"state":MCPState => required,
    error:"error":String => optional,
    tools:"tools":Slice<String> => optional,
    started:"started":String => optional,
});
wire_struct!(JobInfo {
    id:"id":String => required,
    agent:"agent":String => required,
    label:"label":String => required,
    spec:"spec":String => required,
    started:"started":String => required,
    progress:"progress":String => optional,
});
wire_struct!(ChannelInfo {
    board:"board":Option<crate::event::BoardInfo> => optional,
    id:"id":String => required,
    name:"name":String => required,
    dir:"dir":String => required,
    dir_error:"dir_error":String => optional,
    model:"model":String => required,
    root_agent:"root_agent":String => required,
    created:"created":String => required,
    archived:"archived":bool => required,
    seq:"seq":i64 => required,
    live_agents:"live_agents":i64 => required,
    cost_usd:"cost_usd":f64 => float,
    tokens:"tokens":i64 => required,
    budget:"budget":Option<ChannelBudget> => optional,
    trust_pending:"trust_pending":bool => required,
    sandbox:"sandbox":String => optional,
    trust_files:"trust_files":i64 => optional,
    mode:"mode":String => required,
    recap:"recap":i64 => optional,
    state:"state":ChannelState => optional,
    title:"title":String => optional,
    dirs:"dirs":Slice<DirInfo> => optional,
    permissions:"permissions":i64 => optional,
    questions:"questions":i64 => optional,
});
wire_struct!(ChannelBudget {
    calls:"calls":i64 => required,
    tokens:"tokens":i64 => required,
    cost_usd:"cost_usd":f64 => float,
    exhausted:"exhausted":String => optional,
});
wire_struct!(ChannelListParams {
    dir:"dir":String => optional,
    include_archived:"include_archived":bool => optional,
});
wire_struct!(ChannelListResult {
    channels:"channels":Slice<ChannelInfo> => required,
});
wire_struct!(ChannelCreateParams {
    name:"name":String => optional,
    dir:"dir":String => required,
    model:"model":String => optional,
    root_agent:"root_agent":String => optional,
});
wire_struct!(ChannelRef {
    channel:"channel":String => required,
});
wire_struct!(ChannelRenameParams {
    channel:"channel":String => required,
    name:"name":String => required,
});
wire_struct!(ChannelSetModelParams {
    channel:"channel":String => required,
    model:"model":String => required,
});
wire_struct!(ChannelSetModeParams {
    channel:"channel":String => required,
    mode:"mode":String => required,
});
wire_struct!(ChannelSetRecapParams {
    channel:"channel":String => required,
    minutes:"minutes":i64 => required,
});
wire_struct!(ChannelPostParams {
    channel:"channel":String => required,
    text:"text":String => required,
});
wire_struct!(ChannelPostResult {
    to:"to":Slice<String> => required,
});
wire_struct!(ChannelDirParams {
    channel:"channel":String => required,
    dir:"dir":String => required,
});
wire_struct!(PromptInfo {
    id:"id":String => required,
    channel:"channel":String => required,
    agent:"agent":String => optional,
    from:"from":String => optional,
    role:"role":String => optional,
    channel_name:"channel_name":String => optional,
    kind:"kind":PromptKind => required,
    tool:"tool":String => optional,
    input:"input":Option<Box<RawValue>> => raw_optional,
    question:"question":String => optional,
    options:"options":Slice<String> => optional,
    claimed_by:"claimed_by":String => optional,
    escalated:"escalated":bool => required,
    created:"created":String => required,
    dir:"dir":String => optional,
    sticky:"sticky":bool => optional,
    egress:"egress":bool => optional,
    prefix:"prefix":String => optional,
    questions:"questions":Slice<Question> => optional,
    question_number:"question_number":i64 => optional,
    question_total:"question_total":i64 => optional,
});
pub type Question = crate::event::Question;
pub type QuestionOption = crate::event::QuestionOption;
pub type QuestionAnswer = crate::event::QuestionAnswer;
wire_struct!(PromptListParams {
    channel:"channel":String => optional,
});
wire_struct!(PromptListResult {
    prompts:"prompts":Slice<PromptInfo> => required,
});
wire_struct!(PromptClaimParams {
    id:"id":String => required,
});
wire_struct!(PromptReplyParams {
    id:"id":String => required,
    answer:"answer":String => required,
    dir:"dir":String => optional,
    reason:"reason":String => optional,
    answers:"answers":Slice<String> => optional,
    details:"details":Slice<QuestionAnswer> => optional,
});
wire_struct!(TrustStatusParams {
    dir:"dir":String => required,
});
wire_struct!(TrustStatusResult {
    dir:"dir":String => required,
    pending:"pending":bool => required,
    hash:"hash":String => optional,
    files:"files":Slice<String> => optional,
});
wire_struct!(TrustReplyParams {
    dir:"dir":String => required,
    hash:"hash":String => required,
    trust:"trust":bool => required,
});
wire_struct!(SandboxStatus {
    enabled:"enabled":bool => required,
    level:"level":String => required,
    missing:"missing":Slice<String> => optional,
    why:"why":String => optional,
    fix:"fix":String => optional,
});
wire_struct!(SandboxSetParams {
    enabled:"enabled":bool => required,
});
wire_struct!(CommandInfo {
    name:"name":String => required,
    description:"description":String => required,
});
wire_struct!(CommandListResult {
    commands:"commands":Slice<CommandInfo> => required,
});
wire_struct!(CommandRunParams {
    channel:"channel":String => required,
    name:"name":String => required,
    agent:"agent":String => optional,
});
wire_struct!(ConfigScope {
    scope:"scope":String => required,
    channel:"channel":String => optional,
});
wire_struct!(ConfigEntry {
    path:"path":String => required,
    directory:"directory":bool => optional,
});
wire_struct!(ConfigTree {
    root:"root":String => required,
    revision:"revision":String => required,
    files:"files":Slice<ConfigEntry> => required,
});
wire_struct!(ConfigField {
    path:"path":Slice<String> => required,
    kind:"kind":String => required,
    value:"value":String => required,
    set:"set":bool => required,
    choices:"choices":Slice<String> => optional,
});
wire_struct!(ConfigFileParams {
    scope:"scope":String => required,
    channel:"channel":String => optional,
    root:"root":String => required,
    path:"path":String => required,
});
wire_struct!(ConfigDocument {
    path:"path":String => required,
    content:"content":String => required,
    exists:"exists":bool => required,
    fields:"fields":Slice<ConfigField> => optional,
});
wire_struct!(ConfigEditParams {
    scope:"scope":String => required,
    channel:"channel":String => optional,
    root:"root":String => required,
    path:"path":String => required,
    revision:"revision":String => required,
    action:"action":String => required,
    content:"content":String => optional,
    destination:"destination":String => optional,
    field_path:"field_path":Slice<String> => optional,
    field_value:"field_value":Option<Box<RawValue>> => raw_optional,
});
wire_struct!(ConfigEditResult {
    tree:"tree":ConfigTree => required,
    document:"document":Option<ConfigDocument> => optional,
    notice:"notice":String => required,
});
wire_struct!(None {});
wire_struct!(SubscribeResult {
    seq:"seq":i64 => required,
    first:"first":i64 => optional,
});
wire_struct!(UsageSeriesParams {
    channel:"channel":String => optional,
    agent:"agent":String => optional,
    from:"from":Time => time_optional,
    to:"to":Time => time_optional,
    buckets:"buckets":i64 => required,
});
wire_struct!(UsageSeriesResult {
    from:"from":Time => required,
    to:"to":Time => required,
    tokens:"tokens":Slice<i64> => required,
    cost_usd:"cost_usd":Slice<f64> => floats,
});
wire_struct!(PlanUsageResult {
    plans:"plans":Slice<PlanUsageInfo> => required,
});
wire_struct!(PlanUsageInfo {
    provider:"provider":String => required,
    name:"name":String => required,
    plan:"plan":String => optional,
    windows:"windows":Slice<UsageWindowInfo> => required,
    observed:"observed":Time => required,
});
wire_struct!(UsageWindowInfo {
    used_percent:"used_percent":f64 => float,
    minutes:"minutes":i64 => optional,
    resets_at:"resets_at":Time => time_optional,
});
wire_struct!(CacheUsageParams {
    minutes:"minutes":i64 => optional,
});
wire_struct!(CacheUsageResult {
    fresh:"fresh":i64 => required,
    cached:"cached":i64 => required,
    providers:"providers":Slice<CacheProviderUsage> => optional,
});
wire_struct!(CacheProviderUsage {
    provider:"provider":String => required,
    fresh:"fresh":i64 => required,
    cached:"cached":i64 => required,
});
wire_struct!(PlanUsagePoint {
    at:"at":Time => required,
    used_percent:"used_percent":f64 => float,
});
wire_struct!(PlanSeriesParams {
    provider:"provider":String => required,
    from:"from":Time => time_optional,
    to:"to":Time => time_optional,
    buckets:"buckets":i64 => required,
});
wire_struct!(PlanSeriesResult {
    from:"from":Time => required,
    to:"to":Time => required,
    used_percent:"used_percent":Slice<f64> => floats,
});
wire_struct!(ChangedNotification {
    what:"what":String => required,
});
pub type Tier = String;
pub type Kind = String;
pub type AgentState = String;
pub type ChannelState = String;
pub type PromptKind = String;
pub type PromptAction = String;
pub type MCPState = String;
wire_struct!(AttachParams {
    client:"client":String => required,
    tier:"tier":Tier => required,
});
wire_struct!(AttachResult {
    client_id:"client_id":String => required,
    version:"version":i64 => required,
});
wire_struct!(DaemonStatusResult {
    version:"version":i64 => required,
    build:"build":String => required,
    pid:"pid":i64 => required,
    data_dir:"data_dir":String => required,
    channels:"channels":i64 => required,
    agents:"agents":i64 => required,
    working:"working":i64 => required,
    providers:"providers":Slice<String> => required,
});
wire_struct!(DiscordStatus {
    configured:"configured":bool => required,
    enabled:"enabled":bool => required,
    state:"state":String => required,
    bot:"bot":String => optional,
    guild:"guild":String => optional,
    guild_name:"guild_name":String => optional,
    channels:"channels":i64 => required,
    error:"error":String => optional,
    config_path:"config_path":String => required,
});
wire_struct!(WebStatus {
    enabled:"enabled":bool => required,
    url:"url":String => required,
    open_url:"open_url":String => optional,
    error:"error":String => optional,
});
wire_struct!(SheetInfo {
    id:"id":String => required,
    title:"title":String => required,
    author:"author":String => required,
    hash:"hash":String => required,
    size:"size":i64 => required,
    updated:"updated":Time => required,
});
wire_struct!(SheetListResult {
    sheets:"sheets":Slice<SheetInfo> => required,
});
wire_struct!(SubscribeParams {
    channel:"channel":String => required,
    from:"from":i64 => required,
    tail:"tail":i64 => optional,
});
wire_struct!(ReconcileResult {
    channel:"channel":ChannelInfo => required,
    agents:"agents":Slice<AgentInfo> => required,
    prompts:"prompts":Slice<PromptInfo> => required,
    seq:"seq":i64 => required,
});
wire_struct!(Request {
    jsonrpc:"jsonrpc":String => required,
    v:"v":i64 => required,
    id:"id":Option<Box<RawValue>> => optional,
    method:"method":String => required,
    params:"params":Option<Box<RawValue>> => raw_optional,
});
wire_struct!(Response {
    jsonrpc:"jsonrpc":String => required,
    id:"id":Option<Box<RawValue>> => optional,
    result:"result":Option<Box<RawValue>> => raw_optional,
    error:"error":Option<Error> => optional,
    method:"method":String => optional,
    params:"params":Option<Box<RawValue>> => raw_optional,
});
