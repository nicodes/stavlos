//! The small HTML readability pass used by fetch, with a bounded output buffer.
use ego_tree::NodeRef;
use reqwest::Url;
use scraper::{Html, Node};
const LIMIT: usize = 100000 + 4096;
fn tag(node: NodeRef<'_, Node>) -> &str {
    node.value().as_element().map_or("", |e| e.name())
}
fn attr<'a>(node: NodeRef<'a, Node>, key: &str) -> &'a str {
    node.value()
        .as_element()
        .and_then(|e| e.attr(key))
        .unwrap_or("")
}
fn find<'a>(node: NodeRef<'a, Node>, name: &str) -> Option<NodeRef<'a, Node>> {
    node.descendants().find(|n| tag(*n) == name)
}
fn text(node: NodeRef<'_, Node>, raw: bool) -> String {
    let mut text = String::new();
    for node in node.descendants() {
        if let Some(value) = node.value().as_text() {
            text.push_str(&value.text);
        }
        if raw && tag(node) == "br" {
            text.push('\n');
        }
    }
    if raw {
        text
    } else {
        text.split_whitespace().collect::<Vec<_>>().join(" ")
    }
}
pub fn html_to_markdown(source: &[u8], base: Option<&Url>) -> String {
    let doc = Html::parse_document(&String::from_utf8_lossy(source));
    let mut root = find(doc.tree.root(), "body").unwrap_or(doc.tree.root());
    for tag in ["main", "article"] {
        if let Some(found) = find(root, tag) {
            root = found;
            break;
        }
    }
    let mut converter = Converter {
        output: String::new(),
        base,
        quote: 0,
        list: vec![],
    };
    converter.walk(root);
    let mut output = converter.output;
    while output.contains("\n\n\n") {
        output = output.replace("\n\n\n", "\n\n");
    }
    output.trim().into()
}
struct Converter<'a> {
    output: String,
    base: Option<&'a Url>,
    quote: usize,
    list: Vec<(bool, usize)>,
}
enum Step<'a> {
    Node(NodeRef<'a, Node>),
    Flush(bool),
    EndList,
    EndQuote(usize),
}
impl Converter<'_> {
    fn write(&mut self, text: &str) {
        if self.output.len() < LIMIT {
            self.output
                .push_str(crate::clip::head(text, LIMIT - self.output.len()));
        }
    }
    fn flush(&mut self) {
        if !self.output.is_empty() && !self.output.ends_with('\n') {
            self.output
                .truncate(self.output.trim_end_matches(' ').len());
            self.output.push('\n');
        }
    }
    fn block(&mut self, text: &str) {
        if !self.output.is_empty() && !self.output.ends_with("\n\n") {
            self.write(if self.output.ends_with('\n') {
                "\n"
            } else {
                "\n\n"
            });
        }
        if self.quote > 0 {
            self.write(&format!("> {}", text.replace('\n', "\n> ")));
        } else {
            self.write(text);
        }
        self.write("\n\n");
    }
    fn text_node(&mut self, text: &str) {
        let collapsed = text.split_whitespace().collect::<Vec<_>>().join(" ");
        if collapsed.is_empty() {
            if text.contains([' ', '\n', '\t'])
                && !self.output.is_empty()
                && !self.output.ends_with([' ', '\n'])
            {
                self.write(" ");
            }
            return;
        }
        if text.starts_with([' ', '\n'])
            && !self.output.is_empty()
            && !self.output.ends_with([' ', '\n'])
        {
            self.write(" ");
        }
        self.write(&collapsed);
        if text.ends_with([' ', '\n']) {
            self.write(" ");
        }
    }
    fn walk(&mut self, root: NodeRef<'_, Node>) {
        let mut pending = vec![Step::Node(root)];
        while let Some(step) = pending.pop() {
            match step {
                Step::Flush(extra) => {
                    self.flush();
                    if extra {
                        self.write("\n");
                    }
                }
                Step::EndList => {
                    self.list.pop();
                    if self.list.is_empty() {
                        self.write("\n");
                    }
                }
                Step::EndQuote(start) => {
                    self.flush();
                    self.quote -= 1;
                    let inner = self.output[start..].trim_end_matches('\n').to_owned();
                    self.output.truncate(start);
                    let lines = inner
                        .split('\n')
                        .map(|line| {
                            if !line.starts_with('>') && !line.trim().is_empty() {
                                format!("> {line}")
                            } else {
                                line.into()
                            }
                        })
                        .collect::<Vec<_>>()
                        .join("\n");
                    self.write(&(lines + "\n\n"));
                }
                Step::Node(node) => {
                    if self.output.len() >= LIMIT {
                        continue;
                    }
                    if let Some(text) = node.value().as_text() {
                        self.text_node(&text.text);
                        continue;
                    }
                    let name = tag(node);
                    if matches!(
                        name,
                        "script"
                            | "style"
                            | "noscript"
                            | "template"
                            | "svg"
                            | "nav"
                            | "header"
                            | "footer"
                            | "aside"
                            | "iframe"
                            | "button"
                            | "input"
                            | "select"
                            | "textarea"
                            | "head"
                    ) {
                        continue;
                    }
                    match name {
                        "h1" | "h2" | "h3" | "h4" | "h5" | "h6" => {
                            let text = text(node, false);
                            if !text.is_empty() {
                                self.block(&format!(
                                    "{} {text}",
                                    "#".repeat((name.as_bytes()[1] - b'0') as usize)
                                ));
                            }
                            continue;
                        }
                        "p" | "div" | "section" | "article" | "main" | "figure" | "figcaption"
                        | "details" | "summary" | "dd" | "dt" | "address" => {
                            self.flush();
                            pending.push(Step::Flush(matches!(name, "p" | "section" | "article")));
                        }
                        "br" => {
                            self.write("\n");
                            continue;
                        }
                        "hr" => {
                            self.block("---");
                            continue;
                        }
                        "pre" => {
                            let code = text(node, true);
                            let mut language = "";
                            if let Some(code) = find(node, "code") {
                                for class in attr(code, "class").split_whitespace() {
                                    if let Some(value) = class.strip_prefix("language-") {
                                        language = value;
                                    }
                                }
                            }
                            self.block(&format!(
                                "```{language}\n{}\n```",
                                code.trim_end_matches('\n')
                            ));
                            continue;
                        }
                        "code" | "kbd" | "samp" => {
                            let text = text(node, true);
                            if !text.is_empty() {
                                self.write(&format!("`{}`", text.trim().replace('\n', " ")));
                            }
                            continue;
                        }
                        "strong" | "b" | "em" | "i" => {
                            let text = text(node, false);
                            let mark = if matches!(name, "strong" | "b") {
                                "**"
                            } else {
                                "*"
                            };
                            if !text.is_empty() {
                                self.write(&format!("{mark}{text}{mark}"));
                            }
                            continue;
                        }
                        "a" => {
                            let text = text(node, false);
                            let original = attr(node, "href");
                            let href = self
                                .base
                                .filter(|_| !original.is_empty())
                                .and_then(|b| b.join(original).ok())
                                .map(|u| {
                                    if let Some((_, query)) = original.split_once('?') {
                                        let mut resolved = u.clone();
                                        resolved.set_query(None);
                                        resolved.set_fragment(None);
                                        let query = query.split('#').next().unwrap_or("");
                                        format!(
                                            "{resolved}?{query}{}",
                                            u.fragment()
                                                .map(|f| format!("#{f}"))
                                                .unwrap_or_default()
                                        )
                                    } else {
                                        u.to_string()
                                    }
                                })
                                .unwrap_or(original.into());
                            if href.is_empty() || href.starts_with("javascript:") || text == href {
                                self.write(&text);
                            } else if text.is_empty() {
                                self.write(&href);
                            } else {
                                self.write(&format!("[{text}]({href})"));
                            }
                            continue;
                        }
                        "img" => {
                            let alt = attr(node, "alt").trim();
                            if !alt.is_empty() {
                                self.write(&format!("[image: {alt}]"));
                            }
                            continue;
                        }
                        "ul" | "ol" => {
                            self.flush();
                            self.list.push((name == "ol", 0));
                            pending.push(Step::EndList);
                        }
                        "li" => {
                            self.flush();
                            let indent = "  ".repeat(self.list.len().saturating_sub(1));
                            let marker = if let Some((true, number)) = self.list.last_mut() {
                                *number += 1;
                                format!("{number}. ")
                            } else {
                                "- ".into()
                            };
                            self.write(&(indent + &marker));
                            pending.push(Step::Flush(false));
                        }
                        "blockquote" => {
                            self.flush();
                            self.quote += 1;
                            pending.push(Step::EndQuote(self.output.len()));
                        }
                        "table" => {
                            self.flush();
                            let mut rows = Vec::new();
                            for row in node.descendants().filter(|n| tag(*n) == "tr") {
                                let cells = row
                                    .children()
                                    .filter(|c| matches!(tag(*c), "td" | "th"))
                                    .map(|c| text(c, false))
                                    .collect::<Vec<_>>();
                                if !cells.is_empty() {
                                    rows.push(format!("| {} |", cells.join(" | ")));
                                }
                            }
                            if rows.len() > 1 {
                                let cols = rows[0].bytes().filter(|b| *b == b'|').count() - 1;
                                rows.insert(1, format!("|{}", " --- |".repeat(cols)));
                            }
                            if !rows.is_empty() {
                                self.block(&rows.join("\n"));
                            }
                            continue;
                        }
                        _ => {}
                    }
                    pending.extend(node.children().rev().map(Step::Node));
                }
            }
        }
    }
}
