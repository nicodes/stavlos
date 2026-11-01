//! Native owner services. Queues and journal callbacks never wait on a channel actor.
pub mod lock;
pub mod queue;
pub mod stream;
