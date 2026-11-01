use serde_json::{Value, json};
use stavlos::tui::{self, commands, format, keys};
fn cases(raw: &str) -> Vec<Value> {
    serde_json::from_str(raw).unwrap()
}
#[test]
fn command_palette_preserves_all_commands_aliases_exact_matches_and_order() {
    assert_eq!(commands::all().len(), 24);
    for case in cases(include_str!("data/go-tui-palette.json")) {
        let input = case["input"].as_str().unwrap();
        assert_eq!(commands::active(input), case["active"] == true);
        let names = commands::matches(input)
            .iter()
            .map(|command| command.name.as_str())
            .collect::<Vec<_>>();
        assert_eq!(json!(names), case["expected"], "{input}");
    }
    assert_eq!(commands::find("/config").unwrap().name, "/settings");
    assert!(commands::find("/missing").is_none());
}
#[test]
fn list_windows_and_wrap_match_go_and_keyboard_legends_share_the_binding_registry() {
    for case in cases(include_str!("data/go-tui-list-windows.json")) {
        let (start, end) = tui::list_window(
            case["cur"].as_i64().unwrap(),
            case["top"].as_i64().unwrap(),
            case["n"].as_i64().unwrap(),
            case["size"].as_i64().unwrap(),
        );
        assert_eq!(
            (start, end),
            (
                case["start"].as_i64().unwrap(),
                case["end"].as_i64().unwrap()
            )
        );
    }
    for count in 1..20 {
        assert_eq!(tui::wrap_index(count, count), 0);
        assert_eq!(tui::wrap_index(-1, count), count - 1);
    }
    assert!(keys::matches("FocusInput", "ctrl+@"));
    assert!(keys::matches("FocusInput", "ctrl+space"));
    assert_eq!(keys::label(&["NextAgent", "PrevAgent"]), "ctrl+n/p");
    assert_eq!(keys::label(&["SelUp", "SelDown"]), "↑/↓");
    assert_eq!(keys::label(&["Quit"]), "ctrl+c");
}
#[test]
fn numeric_and_time_formats_match_frozen_go_cases() {
    for case in cases(include_str!("data/go-tui-token-format.json")) {
        assert_eq!(format::tokens(case["n"].as_i64().unwrap()), case["tokens"]);
    }
    for case in cases(include_str!("data/go-tui-cost-format.json")) {
        assert_eq!(
            format::cost(case["value"].as_f64().unwrap()),
            case["expected"]
        );
    }
    for case in cases(include_str!("data/go-tui-duration-format.json")) {
        assert_eq!(
            format::elapsed(case["ns"].as_i64().unwrap()),
            case["expected"]
        );
    }
    for case in cases(include_str!("data/go-tui-ago-format.json")) {
        assert_eq!(
            format::ago(
                case["at"].as_str().unwrap().parse().unwrap(),
                case["now"].as_str().unwrap().parse().unwrap()
            ),
            case["expected"]
        );
    }
}
#[test]
fn text_formats_keep_unicode_truncation_and_existing_path_and_identity_display() {
    for case in cases(include_str!("data/go-tui-string-format.json")) {
        let input = case["input"].as_str().unwrap();
        assert_eq!(format::short_id(input), case["id"]);
        assert_eq!(
            format::trunc(input, case["n"].as_u64().unwrap() as usize),
            case["trunc"]
        );
        assert_eq!(format::first_line(input), case["first"]);
    }
    assert_eq!(
        format::short_home("/home/owner/project", "/home/owner"),
        "~/project"
    );
    assert_eq!(format::who("scout", "research"), "scout (research)");
    assert_eq!(format::who("scout", ""), "scout");
}
