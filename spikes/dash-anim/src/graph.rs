use std::collections::HashMap;

use rataflow::{Edge, Flow, Node, NodeContent, NodeRenderContext, StepEdge, Sugiyama};
use ratatui::buffer::Buffer;
use ratatui::layout::Rect;
use ratatui::style::{Color, Modifier, Style};
use ratatui::widgets::{Block, Paragraph, Widget};

use crate::model::{RoleView, WorkerView};

const NODE_W: f64 = 14.0;
const NODE_H: f64 = 3.0;
const ROOT: &str = "dispatcher";

pub fn state_color(state: Option<&str>) -> Color {
    match state {
        Some("working") => Color::Cyan,
        Some("blocked") => Color::Yellow,
        Some("pr_open") => Color::Green,
        Some("done") => Color::Gray,
        Some("failed") => Color::Red,
        _ => Color::DarkGray,
    }
}

#[derive(Debug, Clone)]
pub struct StateNode {
    pub label: String,
    pub state: Option<String>,
}

impl NodeContent for StateNode {
    fn render(&self, ctx: &NodeRenderContext, buf: &mut Buffer) {
        let color = state_color(self.state.as_deref());
        if ctx.area.height < 3 {
            // fit-view zoom can squash a node to two rows, which leaves no room inside a border; the colour carries the state
            let style = Style::default().fg(color).add_modifier(Modifier::REVERSED);
            Paragraph::new(self.label.as_str())
                .style(style)
                .render(ctx.area, buf);
            return;
        }
        let mut block = Block::bordered().border_style(Style::default().fg(color));
        if let Some(state) = &self.state {
            block = block.title(state.as_str());
        }
        let mut style = Style::default().fg(color);
        if ctx.selected {
            style = style.add_modifier(Modifier::BOLD);
        }
        Paragraph::new(self.label.as_str())
            .style(style)
            .block(block)
            .render(ctx.area, buf);
    }
}

pub type GraphFlow = Flow<StateNode, StepEdge>;

pub fn worker_id(w: &WorkerView) -> String {
    format!("w:{}/{}", w.key.0, w.key.1)
}

pub fn role_id(r: &RoleView) -> String {
    format!("r:{}/{}/{}", r.lead_key.0, r.lead_key.1, r.role)
}

/// Roles whose lead is not among `workers` are not drawn.
fn visible_roles<'a>(workers: &[WorkerView], roles: &'a [RoleView]) -> Vec<&'a RoleView> {
    roles
        .iter()
        .filter(|r| workers.iter().any(|w| w.key == r.lead_key))
        .collect()
}

/// Identity of the drawn node set; a change means the layout must be recomputed.
pub fn topology(workers: &[WorkerView], roles: &[RoleView]) -> Vec<String> {
    workers
        .iter()
        .map(worker_id)
        .chain(visible_roles(workers, roles).into_iter().map(role_id))
        .collect()
}

fn animated(state: Option<&str>, reduced_motion: bool) -> bool {
    !reduced_motion && state == Some("working")
}

fn node(id: String, label: &str, state: Option<&str>) -> Node<StateNode> {
    Node::new(
        id,
        (0.0, 0.0),
        (NODE_W, NODE_H),
        StateNode {
            label: label.to_owned(),
            state: state.map(str::to_owned),
        },
    )
}

pub fn layout(horizontal: bool) -> Sugiyama {
    let base = if horizontal {
        Sugiyama::horizontal()
    } else {
        Sugiyama::vertical()
    };
    base.with_node_spacing(1.0).with_rank_spacing(2.0)
}

pub fn build(
    workers: &[WorkerView],
    roles: &[RoleView],
    horizontal: bool,
    reduced_motion: bool,
) -> GraphFlow {
    let mut nodes = vec![node(ROOT.to_owned(), "dispatcher", None)];
    let mut edges = Vec::new();
    for w in workers {
        let id = worker_id(w);
        nodes.push(node(id.clone(), w.label(), Some(&w.state)));
        edges.push(
            Edge::new(format!("e:{ROOT}>{id}"), ROOT, id)
                .with_animated(animated(Some(&w.state), reduced_motion)),
        );
    }
    for r in visible_roles(workers, roles) {
        let id = role_id(r);
        let lead = workers
            .iter()
            .find(|w| w.key == r.lead_key)
            .map(worker_id)
            .expect("visible_roles keeps only roles with a lead");
        nodes.push(node(id.clone(), &r.role, r.state.as_deref()));
        edges.push(
            Edge::new(format!("e:{lead}>{id}"), lead, id)
                .with_animated(animated(r.state.as_deref(), reduced_motion)),
        );
    }
    let mut flow = Flow::with_graph(nodes, edges).expect("ids are unique and edges resolve");
    flow.apply_layout(layout(horizontal));
    let ids: Vec<String> = flow.nodes().map(|n| n.id.clone()).collect();
    for id in ids {
        flow.set_handles_hidden(&id, true);
    }
    flow.request_fit_view();
    flow
}

/// Pushes new states and edge animation into an existing flow with unchanged topology.
pub fn refresh(
    flow: &mut GraphFlow,
    workers: &[WorkerView],
    roles: &[RoleView],
    reduced_motion: bool,
) {
    let mut states: HashMap<String, Option<&str>> = workers
        .iter()
        .map(|w| (worker_id(w), Some(w.state.as_str())))
        .collect();
    for r in visible_roles(workers, roles) {
        states.insert(role_id(r), r.state.as_deref());
    }
    for (id, content) in flow.nodes_content_mut() {
        if let Some(state) = states.get(id) {
            content.state = state.map(str::to_owned);
        }
    }
    let edge_states: Vec<(String, bool)> = workers
        .iter()
        .map(|w| (worker_id(w), &w.state))
        .map(|(id, s)| (format!("e:{ROOT}>{id}"), animated(Some(s), reduced_motion)))
        .chain(visible_roles(workers, roles).into_iter().filter_map(|r| {
            let lead = workers.iter().find(|w| w.key == r.lead_key)?;
            Some((
                format!("e:{}>{}", worker_id(lead), role_id(r)),
                animated(r.state.as_deref(), reduced_motion),
            ))
        }))
        .collect();
    for (id, on) in edge_states {
        flow.set_edge_animated(&id, on);
    }
}

pub fn any_animated(workers: &[WorkerView], roles: &[RoleView], reduced_motion: bool) -> bool {
    workers
        .iter()
        .any(|w| animated(Some(&w.state), reduced_motion))
        || visible_roles(workers, roles)
            .iter()
            .any(|r| animated(r.state.as_deref(), reduced_motion))
}

/// Terminal rect of a node inside the canvas `area`, from the flow's current viewport.
pub fn node_rect(flow: &GraphFlow, id: &str, area: Rect) -> Option<Rect> {
    let n = flow.node(id)?;
    let tl = flow.viewport.world_to_canvas(n.position);
    let br = flow.viewport.world_to_canvas(rataflow::Position::new(
        n.position.x + n.width,
        n.position.y + n.height,
    ));
    let x = area.x as i32 + tl.x.floor() as i32;
    let y = area.y as i32 + tl.y.floor() as i32;
    let w = (br.x.floor() - tl.x.floor()) as i32;
    let h = (br.y.floor() - tl.y.floor()) as i32;
    let rect = Rect::new(
        x.max(0) as u16,
        y.max(0) as u16,
        w.max(0) as u16,
        h.max(0) as u16,
    );
    let r = rect.intersection(area);
    (r.width > 0 && r.height > 0).then_some(r)
}

/// `n` workers with a mix of states and 0-2 roles each; no input source needed.
pub fn synth(n: usize) -> (Vec<WorkerView>, Vec<RoleView>) {
    const STATES: [&str; 6] = ["working", "blocked", "pr_open", "done", "failed", "reaped"];
    const ROLES: [&str; 2] = ["plan-critic", "reviewer"];
    let mut workers = Vec::new();
    let mut roles = Vec::new();
    for i in 0..n {
        let key = ("synth".to_owned(), format!("feat/s{i}"));
        for role in ROLES.iter().take(i % 3) {
            roles.push(RoleView {
                lead_key: key.clone(),
                role: (*role).to_owned(),
                state: Some(if i % 2 == 0 { "working" } else { "blocked" }.to_owned()),
            });
        }
        workers.push(WorkerView {
            key,
            state: STATES[i % STATES.len()].to_owned(),
            name: None,
            detail: None,
        });
    }
    (workers, roles)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn synth_20_builds_with_distinct_positions() {
        let (workers, roles) = synth(20);
        assert_eq!(workers.len(), 20);
        let flow = build(&workers, &roles, false, false);
        assert_eq!(flow.nodes().count(), 1 + 20 + roles.len());
        let mut seen = std::collections::HashSet::new();
        for n in flow.nodes() {
            let p = (n.position.x as i64, n.position.y as i64);
            assert!(seen.insert(p), "duplicate position {p:?} for {}", n.id);
        }
    }

    #[test]
    fn roles_without_a_lead_are_not_drawn() {
        let (workers, mut roles) = synth(3);
        roles.push(RoleView {
            lead_key: ("x".into(), "y".into()),
            role: "ghost".into(),
            state: None,
        });
        assert_eq!(
            topology(&workers, &roles).len(),
            3 + 1 /* synth(3) role at i=1 */ + 2
        );
    }
}
