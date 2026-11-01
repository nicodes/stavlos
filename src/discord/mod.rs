//! The daemon-owned Discord front door, with protocol authority fixed to bridge scope.
pub mod api;
pub mod bridge;
pub mod gateway;
pub mod http;
pub mod interaction;
pub mod permission;
pub mod planning;
pub mod questions;
pub mod runtime;
pub mod service;
pub mod status;
pub mod store;
pub mod text;
mod worker;
