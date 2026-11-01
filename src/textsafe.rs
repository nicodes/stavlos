//! Terminal text from models, files and tools cannot control the terminal or reorder labels.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    Clean,
    Visible,
    Frame,
}
fn rune(bytes: &[u8]) -> (char, usize) {
    let b = bytes[0];
    if b < 128 {
        return (b as char, 1);
    }
    let n = if b & 0xe0 == 0xc0 {
        2
    } else if b & 0xf0 == 0xe0 {
        3
    } else if b & 0xf8 == 0xf0 {
        4
    } else {
        0
    };
    if n > 0
        && bytes.len() >= n
        && let Ok(s) = std::str::from_utf8(&bytes[..n])
    {
        return (s.chars().next().unwrap(), n);
    }
    if (0x80..=0x9f).contains(&b) {
        (b as char, 1)
    } else {
        ('\u{fffd}', 1)
    }
}
fn bidi(c: char) -> bool {
    matches!(c,'\u{202a}'..='\u{202e}'|'\u{2066}'..='\u{2069}'|'\u{200e}'|'\u{200f}'|'\u{061c}')
}
fn invisible(c: char) -> bool {
    matches!(c, '\u{200b}' | '\u{2060}' | '\u{feff}' | '\u{ad}')
}
fn control(c: char) -> bool {
    c != '\n' && c != '\t' && (c < ' ' || ('\u{7f}'..='\u{9f}').contains(&c))
}
fn csi(s: &[u8]) -> usize {
    for (i, c) in s.iter().copied().enumerate() {
        if (0x40..=0x7e).contains(&c) {
            return i + 1;
        }
        if !(0x20..=0x3f).contains(&c) {
            return i;
        }
    }
    s.len()
}
fn string_sequence(s: &[u8]) -> usize {
    for (i, c) in s.iter().copied().enumerate() {
        match c {
            7 => return i + 1,
            27 => {
                return if s.get(i + 1) == Some(&b'\\') {
                    i + 2
                } else {
                    i
                };
            }
            0xc2 if s.get(i + 1) == Some(&0x9c) => return i + 2,
            0x9c => return i + 1,
            b'\n' => return i,
            _ => {}
        }
    }
    s.len()
}
fn sequence(s: &[u8], intro: char) -> usize {
    if intro == '\u{1b}' {
        match s.first() {
            None => 0,
            Some(b'[') => 1 + csi(&s[1..]),
            Some(b']' | b'P' | b'^' | b'_' | b'X') => 1 + string_sequence(&s[1..]),
            Some(b'(' | b')' | b'*' | b'+' | b'#') if s.len() > 1 => 2,
            _ => 1,
        }
    } else if intro == '\u{9b}' {
        csi(s)
    } else {
        string_sequence(s)
    }
}
fn sgr(s: &[u8]) -> usize {
    if s.first() != Some(&b'[') {
        return 0;
    }
    for (i, c) in s.iter().copied().enumerate().skip(1) {
        if c == b'm' {
            return i + 1;
        }
        if !c.is_ascii_digit() && c != b';' && c != b':' {
            return 0;
        }
    }
    0
}
pub fn scrub(s: &[u8], mode: Mode) -> String {
    let mut out = String::with_capacity(s.len());
    let mut i = 0;
    while i < s.len() {
        let (c, n) = rune(&s[i..]);
        if mode == Mode::Frame && c == '\u{1b}' {
            let size = sgr(&s[i + 1..]);
            if size > 0 {
                out.push_str(std::str::from_utf8(&s[i..i + size + 1]).unwrap());
                i += size + 1;
                continue;
            }
        }
        if bidi(c) || (mode == Mode::Visible && invisible(c)) {
            if mode == Mode::Visible {
                out.push_str(&format!("<U+{:04X}>", c as u32));
            }
            i += n;
            continue;
        }
        if !control(c) {
            out.push(c);
            i += n;
            continue;
        }
        if mode == Mode::Visible {
            if c == '\u{7f}' {
                out.push_str("^?")
            } else if c < ' ' {
                out.push('^');
                out.push(char::from_u32('@' as u32 + c as u32).unwrap())
            } else {
                out.push_str(&format!("<{:x}>", c as u32))
            }
        }
        i += n;
        if matches!(
            c,
            '\u{1b}' | '\u{9b}' | '\u{9d}' | '\u{90}' | '\u{9e}' | '\u{9f}' | '\u{98}'
        ) {
            i += sequence(&s[i..], c)
        }
    }
    out
}
pub fn clean(s: &str) -> String {
    scrub(s.as_bytes(), Mode::Clean)
}
pub fn visible(s: &str) -> String {
    scrub(s.as_bytes(), Mode::Visible)
}
pub fn frame(s: &str) -> String {
    scrub(s.as_bytes(), Mode::Frame)
}
