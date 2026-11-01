use crate::{
    model,
    wire::{Slice, Time},
    wire_struct,
};
use serde_json::value::RawValue;
wire_struct!(BoardInfo {source:"source":String=>required,owner:"owner":String=>required,members:"members":Slice<String> =>required});
wire_struct!(ChannelCreatedPayload {board:"board":Option<BoardInfo> =>optional,name:"name":String=>required,dir:"dir":String=>required,model:"model":String=>optional,role:"role":String=>required,mode:"mode":String=>optional});
wire_struct!(ChannelUpdatedPayload {dir:"dir":Option<String> =>optional,name:"name":Option<String> =>optional,model:"model":Option<String> =>optional,mode:"mode":Option<String> =>optional,recap:"recap":Option<i64> =>optional});
wire_struct!(DirPayload {dir:"dir":String=>required,source:"source":String=>optional});
wire_struct!(ChatPayload {kind:"kind":String=>optional,request_id:"request_id":String=>optional,reply_to:"reply_to":Slice<String> =>optional,posts:"posts":Slice<String> =>optional,id:"id":String=>optional,from:"from":String=>optional,text:"text":String=>required,to:"to":Slice<String> =>optional,post:"post":String=>optional});
wire_struct!(AgentSpawnedPayload {directories:"directories":Slice<String> =>optional,id:"id":String=>required,parent:"parent":String=>optional,role:"role":String=>required,name:"name":String=>required,model:"model":String=>optional,variant:"variant":String=>optional,depth:"depth":i64=>required});
wire_struct!(AgentUpdatedPayload {role:"role":Option<String> =>optional,name:"name":Option<String> =>optional,model:"model":Option<String> =>optional,variant:"variant":Option<String> =>optional,reason:"reason":String=>optional});
wire_struct!(Input {channel:"channel":String=>optional,channel_name:"channel_name":String=>optional,expect_response:"expect_response":bool=>optional,request_id:"request_id":String=>optional,reply_to:"reply_to":Slice<String> =>optional,requests:"requests":Slice<ReplyRequest> =>optional,to:"to":Slice<String> =>optional,id:"id":String=>required,kind:"kind":String=>required,text:"text":String=>optional,from:"from":String=>optional,from_name:"from_name":String=>optional,post:"post":String=>optional,job:"job":String=>optional,parties:"parties":Slice<String> =>optional,names:"names":Slice<String> =>optional,recap:"recap":bool=>optional});
wire_struct!(ReplyRequest {channel:"channel":String=>optional,channel_name:"channel_name":String=>optional,id:"id":String=>required,from:"from":String=>required,from_name:"from_name":String=>required,to:"to":Slice<String> =>optional,text:"text":String=>required,post:"post":String=>optional});
wire_struct!(InputTakenPayload {turn:"turn":i64=>required,ids:"ids":Slice<String> =>required});
wire_struct!(TurnPayload {turn:"turn":i64=>required});
wire_struct!(AssistantMessagePayload {turn:"turn":i64=>required,blocks:"blocks":Slice<model::Block> =>required,stop_reason:"stop_reason":String=>required,model:"model":String=>required,usage:"usage":model::Usage=>required,cost_usd:"cost_usd":f64=>float_optional});
wire_struct!(ToolStartedPayload {turn:"turn":i64=>required,call_id:"call_id":String=>required,name:"name":String=>required});
wire_struct!(ToolFinishedPayload {turn:"turn":i64=>required,call_id:"call_id":String=>required,name:"name":String=>required,output:"output":String=>required,is_error:"is_error":bool=>optional,cancelled:"cancelled":bool=>optional,denied:"denied":bool=>optional,instructions:"instructions":Slice<String> =>optional});
wire_struct!(TurnEndedPayload {turn:"turn":i64=>required,reason:"reason":String=>required,error:"error":String=>optional,resume_at:"resume_at":Time=>time_optional,resume:"resume":String=>optional});
wire_struct!(AskRequestedPayload {input:"input":Option<Box<RawValue>> =>raw_optional,dir:"dir":String=>optional,prefix:"prefix":String=>optional,from:"from":String=>optional,role:"role":String=>optional,questions:"questions":Slice<Question> =>optional,question_number:"question_number":i64=>optional,question_total:"question_total":i64=>optional,id:"id":String=>required,kind:"kind":String=>required,call_id:"call_id":String=>optional,tool:"tool":String=>optional,question:"question":String=>optional});
wire_struct!(AskResolvedPayload {reason:"reason":String=>optional,dir:"dir":String=>optional,answers:"answers":Slice<String> =>optional,details:"details":Slice<QuestionAnswer> =>optional,id:"id":String=>required,outcome:"outcome":String=>required,answer:"answer":String=>optional,by:"by":String=>optional});
wire_struct!(Question {question:"question":String=>required,options:"options":Slice<QuestionOption> =>required});
wire_struct!(QuestionOption {label:"label":String=>required,description:"description":String=>optional});
wire_struct!(QuestionAnswer {selected:"selected":Slice<i64> =>optional,custom:"custom":String=>optional});
wire_struct!(PermitPayload {tool:"tool":String=>required,call:"call":String=>optional,prefix:"prefix":String=>optional});
wire_struct!(JobStartedPayload {id:"id":String=>required,command:"command":String=>required});
wire_struct!(JobFinishedPayload {id:"id":String=>required,summary:"summary":String=>required,output:"output":String=>optional,exit_code:"exit_code":i64=>required,is_error:"is_error":bool=>optional});
wire_struct!(JobStoppedPayload {id:"id":String=>required,reason:"reason":String=>optional});
wire_struct!(TodoItem {id:"id":String=>required,text:"text":String=>required,status:"status":String=>required});
wire_struct!(TodoPayload {items:"items":Slice<TodoItem> =>required});
wire_struct!(McpStartedPayload {server:"server":String=>required,tools:"tools":Slice<String> =>required});
wire_struct!(McpFailedPayload {server:"server":String=>required,error:"error":String=>required});
wire_struct!(McpRefPayload {server:"server":String=>required});
wire_struct!(CompactionPayload {from_seq:"from_seq":i64=>optional,to_seq:"to_seq":i64=>optional,summary:"summary":String=>optional,before:"before":i64=>optional,after:"after":i64=>optional,error:"error":String=>optional});
wire_struct!(SheetPayload {id:"id":String=>required,title:"title":String=>optional,author:"author":String=>optional,hash:"hash":String=>optional,size:"size":i64=>optional});
// Go omitempty does not omit a value-typed struct; zero usage is deliberately retained.
wire_struct!(ModelCallPayload {purpose:"purpose":String=>required,usage:"usage":model::Usage=>required,cost_usd:"cost_usd":f64=>float_optional});

pub const INPUT_PROMPT: &str = "prompt";
pub const INPUT_STEER: &str = "steer";
pub const INPUT_REQUEST: &str = "request";
pub const INPUT_AGENT_STEER: &str = "agent_steer";
pub const INPUT_INFO: &str = "info";
pub const INPUT_RESPONSE: &str = "response";
pub const INPUT_JOB: &str = "job";
pub const INPUT_REMINDER: &str = "reminder";
pub const INPUT_RESUME: &str = "resume";
pub const REASON_END_TURN: &str = "end_turn";
pub const REASON_CANCELLED: &str = "cancelled";
pub const REASON_ERROR: &str = "error";
pub const REASON_MAX_TOKENS: &str = "max_tokens";
pub const RESUME_LIMIT: &str = "limit";
pub const RESUME_FAULT: &str = "fault";
pub const ASK_ANSWERED: &str = "answered";
pub const ASK_DEFAULTED: &str = "defaulted";
pub const ASK_WITHDRAWN: &str = "withdrawn";
pub const TODO_PENDING: &str = "pending";
pub const TODO_IN_PROGRESS: &str = "in_progress";
pub const TODO_DONE: &str = "done";
pub const TODO_CANCELLED: &str = "cancelled";
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, serde::Serialize)]
pub struct InputRule {
    pub wakes: bool,
    pub mid_turn: bool,
    pub harness: bool,
}
pub fn input_rule(kind: &str) -> InputRule {
    match kind {
        INPUT_STEER | INPUT_REQUEST | INPUT_AGENT_STEER => InputRule {
            wakes: true,
            mid_turn: true,
            harness: false,
        },
        INPUT_INFO => InputRule {
            wakes: false,
            mid_turn: true,
            harness: false,
        },
        INPUT_REMINDER | INPUT_RESUME => InputRule {
            wakes: true,
            mid_turn: false,
            harness: true,
        },
        _ => InputRule {
            wakes: true,
            mid_turn: false,
            harness: false,
        },
    }
}
pub fn input_kinds() -> &'static [&'static str] {
    &[
        INPUT_AGENT_STEER,
        INPUT_INFO,
        INPUT_JOB,
        INPUT_PROMPT,
        INPUT_REMINDER,
        INPUT_REQUEST,
        INPUT_RESPONSE,
        INPUT_RESUME,
        INPUT_STEER,
    ]
}
