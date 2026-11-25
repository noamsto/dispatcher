use std::time::Duration;

use ratatui::buffer::Buffer;
use ratatui::layout::Rect;
use ratatui::style::Color;
use tachyonfx::{fx, Effect, EffectManager, EffectTimer, Interpolation, Motion};

pub const GLOW_MS: u32 = 600;
pub const APPEAR_MS: u32 = 350;
pub const VANISH_MS: u32 = 350;

/// Effect manager that does nothing under `--reduced-motion`.
pub struct Fx {
    manager: EffectManager<String>,
    reduced_motion: bool,
}

impl Fx {
    pub fn new(reduced_motion: bool) -> Self {
        Self {
            manager: EffectManager::default(),
            reduced_motion,
        }
    }

    fn add(&mut self, key: String, area: Rect, effect: Effect) {
        if self.reduced_motion {
            return;
        }
        self.manager.add_unique_effect(key, effect.with_area(area));
    }

    /// Flash a node rect from `color` back to its normal look.
    pub fn glow(&mut self, key: &str, area: Rect, color: Color) {
        let timer = EffectTimer::from_ms(GLOW_MS, Interpolation::SineOut);
        self.add(
            format!("glow:{key}"),
            area,
            fx::fade_from(color, color, timer),
        );
    }

    pub fn appear(&mut self, key: &str, area: Rect) {
        let timer = EffectTimer::from_ms(APPEAR_MS, Interpolation::QuadOut);
        self.add(
            format!("row:{key}"),
            area,
            fx::slide_in(Motion::LeftToRight, 8, 0, Color::Black, timer),
        );
    }

    pub fn vanish(&mut self, key: &str, area: Rect) {
        let timer = EffectTimer::from_ms(VANISH_MS, Interpolation::QuadIn);
        self.add(
            format!("row:{key}"),
            area,
            fx::fade_to(Color::Black, Color::Black, timer),
        );
    }

    pub fn is_running(&self) -> bool {
        self.manager.is_running()
    }

    pub fn process(&mut self, dt: Duration, buf: &mut Buffer, area: Rect) {
        self.manager.process_effects(dt.into(), buf, area);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reduced_motion_glow_is_not_running() {
        let mut f = Fx::new(true);
        f.glow("n", Rect::new(0, 0, 4, 3), Color::Cyan);
        assert!(!f.is_running());
    }

    #[test]
    fn normal_glow_is_running_then_finishes() {
        let mut f = Fx::new(false);
        let area = Rect::new(0, 0, 4, 3);
        f.glow("n", area, Color::Cyan);
        assert!(f.is_running());
        let mut buf = Buffer::empty(area);
        f.process(
            Duration::from_millis(u64::from(GLOW_MS) + 50),
            &mut buf,
            area,
        );
        assert!(!f.is_running());
    }
}
