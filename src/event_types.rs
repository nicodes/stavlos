// The existing event vocabulary; new variants require every state/history match to be updated.
macro_rules! event_vocabulary {
    ($($constant:ident,$variant:ident=>$value:literal),*$(,)?) => {
        $(pub const $constant:&str=$value;)*
        #[derive(Clone,Copy,Debug,Default,PartialEq,Eq)]
        pub enum Kind {$($variant,)* #[default] Unknown}
        impl Kind {pub fn parse(value:&str)->Self {match value {$($value=>Self::$variant,)*_=>Self::Unknown}}}
        pub const ALL_TYPES:&[&str]=&[$($value),*];
    };
}
event_vocabulary! {
    CHANNEL_CREATED,ChannelCreated=>"channel.created",
    CHANNEL_UPDATED,ChannelUpdated=>"channel.updated",
    CHANNEL_ARCHIVED,ChannelArchived=>"channel.archived",
    CHANNEL_DIR_ADDED,ChannelDirAdded=>"channel.dir_added",
    CHANNEL_DIR_REMOVED,ChannelDirRemoved=>"channel.dir_removed",
    CHAT_POSTED,ChatPosted=>"chat.posted",
    CHAT_MESSAGE,ChatMessage=>"chat.message",
    AGENT_SPAWNED,AgentSpawned=>"agent.spawned",
    AGENT_UPDATED,AgentUpdated=>"agent.updated",
    AGENT_KILLED,AgentKilled=>"agent.killed",
    AGENT_CANCELLED,AgentCancelled=>"agent.cancelled",
    INPUT_QUEUED,InputQueued=>"input.queued",
    INPUT_TAKEN,InputTaken=>"input.taken",
    MODEL_CALL_STARTED,ModelCallStarted=>"model.call_started",
    MODEL_CALL_COMPLETED,ModelCallCompleted=>"model.call_completed",
    TURN_STARTED,TurnStarted=>"turn.started",
    ASSISTANT_MESSAGE,AssistantMessage=>"assistant.message",
    TOOL_STARTED,ToolStarted=>"tool.started",
    TOOL_FINISHED,ToolFinished=>"tool.finished",
    TURN_ENDED,TurnEnded=>"turn.ended",
    TURN_ABORTED,TurnAborted=>"turn.aborted",
    ASK_REQUESTED,AskRequested=>"ask.requested",
    ASK_RESOLVED,AskResolved=>"ask.resolved",
    PERMIT_GRANTED,PermitGranted=>"permit.granted",
    JOB_STARTED,JobStarted=>"job.started",
    JOB_FINISHED,JobFinished=>"job.finished",
    JOB_STOPPED,JobStopped=>"job.stopped",
    TODO_CHANGED,TodoChanged=>"todo.changed",
    SHEET_WRITTEN,SheetWritten=>"sheet.written",
    SHEET_DELETED,SheetDeleted=>"sheet.deleted",
    MCPSTARTED,McpStarted=>"mcp.started",
    MCPFAILED,McpFailed=>"mcp.failed",
    MCPSTOPPED,McpStopped=>"mcp.stopped",
    COMPACTION_STARTED,CompactionStarted=>"compaction.started",
    COMPACTION_DONE,CompactionDone=>"compaction.done",
    COMPACTION_FAILED,CompactionFailed=>"compaction.failed",
}
