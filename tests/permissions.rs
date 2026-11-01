use serde::Deserialize;
use stavlos::{
    policy::{Kind, Layered, Rule, Set, Subject, Verb},
    shellcmd,
};

#[derive(Deserialize)]
struct ShellCase {
    command: String,
    words: Option<Vec<String>>,
    ok: bool,
    simple: bool,
    prefix: String,
    commands: Vec<String>,
    covers_self: bool,
}
#[test]
fn every_original_go_shell_classifier_and_permission_prefix_matches() {
    let cases: Vec<ShellCase> =
        serde_json::from_str(include_str!("data/go-shell-policy-commands.json")).unwrap();
    assert!(cases.len() >= 490);
    for case in cases {
        let actual = shellcmd::words(&case.command);
        assert_eq!(
            actual.is_some(),
            case.ok,
            "words accepted {:?}",
            case.command
        );
        assert_eq!(actual, case.words, "words {:?}", case.command);
        assert_eq!(
            shellcmd::simple(&case.command),
            case.simple,
            "simple {:?}",
            case.command
        );
        let prefix = shellcmd::prefix(&case.command);
        assert_eq!(prefix, case.prefix, "prefix {:?}", case.command);
        assert_eq!(
            shellcmd::commands(&case.command),
            case.commands,
            "embedded commands {:?}",
            case.command
        );
        assert_eq!(
            shellcmd::covers(&prefix, &case.command),
            case.covers_self,
            "remembered permission {:?}",
            case.command
        );
    }
}
#[derive(Deserialize)]
struct PolicyCase {
    rules: Vec<Rule>,
    overlay: Vec<Rule>,
    tool: String,
    kind: u8,
    values: Vec<String>,
    decision: Verb,
    layered: Verb,
    about: String,
    lookup: String,
    matched: bool,
}
#[test]
fn original_go_policy_specificity_and_tightening_judgments_match() {
    let cases: Vec<PolicyCase> =
        serde_json::from_str(include_str!("data/go-policy-decisions.json")).unwrap();
    assert!(cases.len() >= 900);
    for case in cases {
        let kind = match case.kind {
            0 => Kind::Text,
            1 => Kind::Command,
            2 => Kind::Path,
            3 => Kind::Url,
            4 => Kind::Id,
            _ => panic!("invalid fixture kind"),
        };
        let subject = Subject::new(kind, case.values);
        let base = Set::new(case.rules);
        assert_eq!(
            base.decide(&case.tool, &subject),
            case.decision,
            "base {:?} {:?}",
            case.tool,
            subject
        );
        let lookup = base.lookup(&case.tool, kind, subject.primary());
        assert_eq!(
            lookup.is_some(),
            case.matched,
            "lookup presence {:?}",
            subject
        );
        if let Some(verb) = lookup {
            assert_eq!(
                serde_json::to_value(verb).unwrap().as_str().unwrap(),
                case.lookup
            );
        }
        assert_eq!(
            Layered::new(base, [Set::new(case.overlay)]).decide(&case.tool, &subject),
            (case.layered, case.about),
            "layered {:?} {:?}",
            case.tool,
            subject
        );
    }
}
#[test]
fn merging_rules_replaces_only_exact_keys_and_overlay_cannot_loosen() {
    let rule = |tool: &str, pattern: &str, verb| Rule {
        tool: tool.into(),
        pattern: pattern.into(),
        verb,
    };
    let base = Set::new([
        rule("shell", "*", Verb::Allow),
        rule("shell", "git push*", Verb::Ask),
    ]);
    let overlay = Set::new([
        rule("shell", "git push*", Verb::Allow),
        rule("shell", "curl*", Verb::Deny),
    ]);
    let subject = Subject::command("git push main");
    assert_eq!(base.merge(&overlay).decide("shell", &subject), Verb::Allow);
    let layered = Layered::new(base, [overlay, Set::default()]);
    assert_eq!(layered.overlays.len(), 1);
    assert_eq!(layered.decide("shell", &subject).0, Verb::Ask);
    assert!(!shellcmd::covers("go test", "go test; rm x"));
    assert!(!shellcmd::covers("ls", "FOO=1 ls"));
    assert!(!shellcmd::covers("go test", "go testx"));
    assert!(shellcmd::covers("git commit", r#"git commit -m "a; b""#));
}
