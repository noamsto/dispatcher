use std::time::Duration;

use ratatui::buffer::Buffer;
use ratatui::layout::Rect;
use ratatui::style::{Color, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Paragraph, Widget};

use crate::diff::AttentionChange;
use crate::fx::{Fx, VANISH_MS};
use crate::model::AttentionItem;

#[derive(Debug, Clone, Copy, PartialEq)]
enum Pending {
    Appear,
    Vanish,
}

#[derive(Debug, Clone)]
pub struct Row {
    pub item: AttentionItem,
    /// Time left before a removed row is dropped; `None` while the item is live.
    dying: Option<Duration>,
    pending: Option<Pending>,
}

/// Attention rows; removed items stay visible while their fade-out plays.
#[derive(Debug, Default)]
pub struct ListState {
    pub rows: Vec<Row>,
}

impl ListState {
    pub fn apply(&mut self, changes: &[AttentionChange], reduced_motion: bool) {
        for change in changes {
            match change {
                AttentionChange::Added(item) => {
                    self.rows.retain(|r| r.item.id != item.id);
                    self.rows.push(Row {
                        item: item.clone(),
                        dying: None,
                        pending: Some(Pending::Appear),
                    });
                }
                AttentionChange::Removed(item) if reduced_motion => {
                    self.rows.retain(|r| r.item.id != item.id);
                }
                AttentionChange::Removed(item) => {
                    if let Some(row) = self.rows.iter_mut().find(|r| r.item.id == item.id) {
                        row.dying = Some(Duration::from_millis(u64::from(VANISH_MS)));
                        row.pending = Some(Pending::Vanish);
                    }
                }
            }
        }
    }

    /// Drops removed rows whose fade-out has finished.
    pub fn tick(&mut self, dt: Duration) {
        for row in &mut self.rows {
            row.dying = row.dying.map(|left| left.saturating_sub(dt));
        }
        self.rows.retain(|r| r.dying != Some(Duration::ZERO));
    }

    pub fn has_dying(&self) -> bool {
        self.rows.iter().any(|r| r.dying.is_some())
    }

    /// Draws the rows and fires the effects queued since the last draw.
    pub fn render(&mut self, area: Rect, buf: &mut Buffer, fx: &mut Fx) {
        for (i, row) in self.rows.iter_mut().enumerate() {
            let Ok(dy) = u16::try_from(i) else { break };
            if dy >= area.height {
                break;
            }
            let rect = Rect::new(area.x, area.y + dy, area.width, 1);
            let color = match row.item.severity.as_str() {
                "critical" => Color::Red,
                "warning" => Color::Yellow,
                "ready" => Color::Green,
                _ => Color::DarkGray,
            };
            let line = Line::from(vec![
                Span::styled(
                    format!("{:<9}", row.item.severity),
                    Style::default().fg(color),
                ),
                Span::styled(
                    format!("{:<14}", row.item.kind),
                    Style::default().fg(Color::Gray),
                ),
                Span::raw(row.item.message.as_str()),
            ]);
            Paragraph::new(line).render(rect, buf);
            match row.pending.take() {
                Some(Pending::Appear) => fx.appear(&row.item.id, rect),
                Some(Pending::Vanish) => fx.vanish(&row.item.id, rect),
                None => {}
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn item(id: &str) -> AttentionItem {
        AttentionItem {
            id: id.into(),
            severity: "warning".into(),
            kind: "k".into(),
            message: "m".into(),
        }
    }

    #[test]
    fn removed_row_is_retained_then_dropped() {
        let mut s = ListState::default();
        s.apply(
            &[
                AttentionChange::Added(item("a")),
                AttentionChange::Added(item("b")),
            ],
            false,
        );
        assert_eq!(s.rows.len(), 2);

        s.apply(&[AttentionChange::Removed(item("a"))], false);
        assert_eq!(s.rows.len(), 2, "kept while fading");
        assert_eq!(s.rows.iter().filter(|r| r.dying.is_none()).count(), 1);
        assert!(s.has_dying());

        s.tick(Duration::from_millis(100));
        assert_eq!(s.rows.len(), 2);
        s.tick(Duration::from_millis(u64::from(VANISH_MS)));
        assert_eq!(s.rows.len(), 1);
        assert!(!s.has_dying());
    }

    #[test]
    fn reduced_motion_drops_removed_rows_immediately() {
        let mut s = ListState::default();
        s.apply(&[AttentionChange::Added(item("a"))], true);
        s.apply(&[AttentionChange::Removed(item("a"))], true);
        assert!(s.rows.is_empty());
    }
}
