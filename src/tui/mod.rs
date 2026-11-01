//! Native terminal state and Ratatui presentation.
pub mod commands;
pub mod controller;
pub mod driver;
pub mod editor;
pub mod format;
pub mod keys;
pub mod render;
pub mod transcript;
pub fn wrap_index(index: i64, count: i64) -> i64 {
    if count <= 0 {
        0
    } else {
        index.rem_euclid(count)
    }
}
pub fn list_window(cursor: i64, top: i64, count: i64, size: i64) -> (i64, i64) {
    let start = 0.max(top.max(cursor - size + 1).min(cursor));
    (start, (start + size).min(count))
}
