//! UTF-8 input editing uses grapheme boundaries; file contents stay separate from display escaping.
use unicode_segmentation::UnicodeSegmentation;
use unicode_width::UnicodeWidthStr;
#[derive(Clone, Default)]
pub struct Editor {
    pub text: String,
    pub cursor: usize,
    pub secret: bool,
    preferred: Option<usize>,
}
impl Editor {
    pub fn new(text: impl Into<String>) -> Self {
        let text = text.into();
        let cursor = text.len();
        Self {
            text,
            cursor,
            ..Default::default()
        }
    }
    pub fn clear(&mut self) {
        self.text.clear();
        self.cursor = 0;
        self.preferred = None;
    }
    pub fn set(&mut self, text: impl Into<String>) {
        self.text = text.into();
        self.cursor = self.text.len();
        self.preferred = None;
    }
    fn previous(&self) -> usize {
        self.text[..self.cursor]
            .grapheme_indices(true)
            .next_back()
            .map(|(index, _)| index)
            .unwrap_or(0)
    }
    fn next(&self) -> usize {
        self.text[self.cursor..]
            .graphemes(true)
            .next()
            .map(|grapheme| self.cursor + grapheme.len())
            .unwrap_or(self.text.len())
    }
    pub fn line_start(&self) -> usize {
        self.text[..self.cursor]
            .rfind('\n')
            .map(|index| index + 1)
            .unwrap_or(0)
    }
    pub fn line_end(&self) -> usize {
        self.text[self.cursor..]
            .find('\n')
            .map(|index| self.cursor + index)
            .unwrap_or(self.text.len())
    }
    pub fn column(&self) -> usize {
        UnicodeWidthStr::width(&self.text[self.line_start()..self.cursor])
    }
    pub fn row(&self) -> usize {
        self.text[..self.cursor]
            .bytes()
            .filter(|byte| *byte == b'\n')
            .count()
    }
    pub fn insert(&mut self, input: &str) {
        let input = input.replace("\r\n", "\n").replace('\r', "\n");
        let input = crate::textsafe::clean(&input);
        self.text.insert_str(self.cursor, &input);
        self.cursor += input.len();
        self.preferred = None;
    }
    pub fn vertical(&mut self, delta: i32) -> bool {
        let start = self.line_start();
        let end = self.line_end();
        let column = self.preferred.unwrap_or_else(|| self.column());
        let (start, end) = if delta < 0 {
            if start == 0 {
                return false;
            }
            let end = start - 1;
            let start = self.text[..end]
                .rfind('\n')
                .map(|index| index + 1)
                .unwrap_or(0);
            (start, end)
        } else {
            if end == self.text.len() {
                return false;
            }
            let start = end + 1;
            let end = self.text[start..]
                .find('\n')
                .map(|index| start + index)
                .unwrap_or(self.text.len());
            (start, end)
        };
        let mut width = 0;
        let mut cursor = start;
        for grapheme in self.text[start..end].graphemes(true) {
            let next = UnicodeWidthStr::width(grapheme);
            if width + next > column {
                break;
            }
            width += next;
            cursor += grapheme.len();
        }
        self.cursor = cursor;
        self.preferred = Some(column);
        true
    }
    pub fn visual_vertical(&mut self, delta: i32, width: usize) -> bool {
        let width = width.max(1);
        let mut points = vec![(0_usize, 0_usize, 0_usize)];
        let (mut row, mut column) = (0, 0);
        for (index, grapheme) in self.text.grapheme_indices(true) {
            if grapheme == "\n" {
                row += 1;
                column = 0;
            } else {
                let columns = UnicodeWidthStr::width(grapheme).min(width);
                if column + columns > width {
                    row += 1;
                    column = 0;
                }
                column += columns;
            }
            points.push((index + grapheme.len(), row, column));
        }
        let (_, current, column) = points
            .iter()
            .find(|(index, _, _)| *index == self.cursor)
            .copied()
            .unwrap_or_default();
        let preferred = self.preferred.unwrap_or(column);
        let target = if delta < 0 {
            let Some(row) = current.checked_sub(1) else {
                return false;
            };
            row
        } else {
            current + 1
        };
        let Some((cursor, _, _)) = points
            .iter()
            .filter(|(_, row, _)| *row == target)
            .take_while(|(_, _, column)| *column <= preferred)
            .last()
            .or_else(|| points.iter().find(|(_, row, _)| *row == target))
            .copied()
        else {
            return false;
        };
        self.cursor = cursor;
        self.preferred = Some(preferred);
        true
    }
    pub fn key(&mut self, key: &str) -> bool {
        match key {
            "left" | "ctrl+b" => self.cursor = self.previous(),
            "right" | "ctrl+f" => self.cursor = self.next(),
            "home" | "ctrl+a" => self.cursor = self.line_start(),
            "end" | "ctrl+e" => self.cursor = self.line_end(),
            "ctrl+home" => self.cursor = 0,
            "ctrl+end" => self.cursor = self.text.len(),
            "up" => return self.vertical(-1),
            "down" => return self.vertical(1),
            "backspace" | "ctrl+h" => {
                let previous = self.previous();
                self.text.replace_range(previous..self.cursor, "");
                self.cursor = previous;
            }
            "delete" | "ctrl+d" => {
                let next = self.next();
                self.text.replace_range(self.cursor..next, "");
            }
            "ctrl+u" => {
                let start = self.line_start();
                self.text.replace_range(start..self.cursor, "");
                self.cursor = start;
            }
            "ctrl+k" => {
                let end = self.line_end();
                let end = if end == self.cursor && end < self.text.len() {
                    end + 1
                } else {
                    end
                };
                self.text.replace_range(self.cursor..end, "");
            }
            "alt+left" | "alt+b" | "ctrl+left" => self.word_left(),
            "alt+right" | "alt+f" | "ctrl+right" => self.word_right(),
            "ctrl+w" | "alt+backspace" => {
                let end = self.cursor;
                self.word_left();
                self.text.replace_range(self.cursor..end, "");
            }
            "alt+d" => {
                let start = self.cursor;
                self.word_right();
                self.text.replace_range(start..self.cursor, "");
                self.cursor = start;
            }
            "ctrl+j" | "alt+enter" => self.insert("\n"),
            _ => return false,
        }
        self.preferred = None;
        true
    }
    fn word_left(&mut self) {
        while self.cursor > 0
            && self.text[..self.cursor]
                .chars()
                .next_back()
                .is_some_and(char::is_whitespace)
        {
            self.cursor = self.previous();
        }
        while self.cursor > 0
            && self.text[..self.cursor]
                .chars()
                .next_back()
                .is_some_and(|ch| !ch.is_whitespace())
        {
            self.cursor = self.previous();
        }
    }
    fn word_right(&mut self) {
        while self.cursor < self.text.len()
            && self.text[self.cursor..]
                .chars()
                .next()
                .is_some_and(char::is_whitespace)
        {
            self.cursor = self.next();
        }
        while self.cursor < self.text.len()
            && self.text[self.cursor..]
                .chars()
                .next()
                .is_some_and(|ch| !ch.is_whitespace())
        {
            self.cursor = self.next();
        }
    }
    pub fn display(&self) -> String {
        if self.secret {
            "•".repeat(self.text.graphemes(true).count())
        } else {
            crate::textsafe::visible(&self.text)
        }
    }
    pub fn display_before_cursor(&self) -> String {
        if self.secret {
            "•".repeat(self.text[..self.cursor].graphemes(true).count())
        } else {
            crate::textsafe::visible(&self.text[..self.cursor])
        }
    }
}
