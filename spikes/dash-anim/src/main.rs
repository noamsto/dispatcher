mod diff;
mod fx;
mod graph;
mod list;
mod model;
mod source;

use std::io::{self, Stdout};
use std::path::PathBuf;
use std::sync::mpsc::{self, RecvTimeoutError};
use std::time::{Duration, Instant};

use clap::{Parser, ValueEnum};
use crossterm::event::{KeyCode, KeyModifiers};
use crossterm::execute;
use crossterm::terminal::{
    disable_raw_mode, enable_raw_mode, EnterAlternateScreen, LeaveAlternateScreen,
};
use ratatui::backend::{CrosstermBackend, TestBackend};
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Paragraph, Widget};
use ratatui::{Frame, Terminal};

use diff::{diff_attention, diff_workers, WorkerChange};
use fx::Fx;
use graph::{state_color, GraphFlow};
use list::ListState;
use model::{AttentionItem, RoleView, Snapshot, WorkerView};
use source::Msg;

const FX_TICK: Duration = Duration::from_millis(33);
const EDGE_TICK: Duration = Duration::from_millis(120);

#[derive(Clone, Copy, PartialEq, ValueEnum)]
enum Tab {
    Graph,
    List,
}

#[derive(Parser)]
struct Args {
    /// Replay a JSON document stream; repeatable. Without it (and without --synthetic), spawn `crew dash`.
    #[arg(long)]
    fixture: Vec<PathBuf>,
    #[arg(long, default_value_t = 1500)]
    replay_ms: u64,
    /// Generate N synthetic workers instead of reading any input source.
    #[arg(long, value_name = "N")]
    synthetic: Option<usize>,
    /// Lay the graph out left to right.
    #[arg(long)]
    horizontal: bool,
    /// Disable effects and edge animation; removed rows drop immediately.
    #[arg(long)]
    reduced_motion: bool,
    /// Render one frame headlessly as text, e.g. `160x50`, and exit.
    #[arg(long, value_name = "WxH", value_parser = parse_size)]
    dump: Option<(u16, u16)>,
    /// Tab shown first (or dumped).
    #[arg(long, value_enum, default_value = "graph")]
    tab: Tab,
}

fn parse_size(s: &str) -> Result<(u16, u16), String> {
    let (w, h) = s.split_once('x').ok_or("expected WxH")?;
    let num = |v: &str| v.parse::<u16>().map_err(|e| e.to_string());
    Ok((num(w)?, num(h)?))
}

struct App {
    tab: Tab,
    horizontal: bool,
    reduced_motion: bool,
    workers: Vec<WorkerView>,
    roles: Vec<RoleView>,
    attention: Vec<AttentionItem>,
    flow: Option<GraphFlow>,
    topology: Vec<String>,
    list: ListState,
    fx: Fx,
    /// Node ids whose glow is fired at the next draw, once the viewport is final.
    pending_glow: Vec<(String, ratatui::style::Color)>,
}

impl App {
    fn new(args: &Args) -> Self {
        Self {
            tab: args.tab,
            horizontal: args.horizontal,
            reduced_motion: args.reduced_motion,
            workers: Vec::new(),
            roles: Vec::new(),
            attention: Vec::new(),
            flow: None,
            topology: Vec::new(),
            list: ListState::default(),
            fx: Fx::new(args.reduced_motion),
            pending_glow: Vec::new(),
        }
    }

    fn set_graph_data(&mut self, workers: Vec<WorkerView>, roles: Vec<RoleView>) {
        for c in diff_workers(&self.workers, &workers) {
            let (w, active) = match &c {
                WorkerChange::Added(w) => (w, true),
                WorkerChange::Changed { worker, .. } => (worker, true),
                WorkerChange::Removed(w) => (w, false),
            };
            if active {
                self.pending_glow
                    .push((graph::worker_id(w), state_color(Some(&w.state))));
            }
        }
        for r in &roles {
            let old = self
                .roles
                .iter()
                .find(|o| graph::role_id(o) == graph::role_id(r));
            if old.is_some_and(|o| o.state != r.state) {
                self.pending_glow
                    .push((graph::role_id(r), state_color(r.state.as_deref())));
            }
        }
        self.workers = workers;
        self.roles = roles;
        let topo = graph::topology(&self.workers, &self.roles);
        match &mut self.flow {
            Some(flow) if topo == self.topology => {
                graph::refresh(flow, &self.workers, &self.roles, self.reduced_motion);
            }
            _ => {
                self.flow = Some(graph::build(
                    &self.workers,
                    &self.roles,
                    self.horizontal,
                    self.reduced_motion,
                ));
                self.topology = topo;
            }
        }
    }

    fn on_doc(&mut self, snap: Snapshot) {
        match snap {
            Snapshot::Map { workers } => {
                let roles = std::mem::take(&mut self.roles);
                self.set_graph_data(workers, roles);
            }
            Snapshot::Overview { attention, roles } => {
                let changes = diff_attention(&self.attention, &attention);
                self.list.apply(&changes, self.reduced_motion);
                self.attention = attention;
                let workers = std::mem::take(&mut self.workers);
                self.set_graph_data(workers, roles);
            }
        }
    }

    fn animating_edges(&self) -> bool {
        graph::any_animated(&self.workers, &self.roles, self.reduced_motion)
    }

    fn effects_active(&self) -> bool {
        self.fx.is_running() || self.list.has_dying()
    }

    fn draw(&mut self, frame: &mut Frame, dt: Duration) {
        let [header, body] =
            Layout::vertical([Constraint::Length(1), Constraint::Min(0)]).areas(frame.area());
        self.render_header(header, frame.buffer_mut());
        match self.tab {
            Tab::Graph => self.render_graph(body, frame),
            Tab::List => self.list.render(body, frame.buffer_mut(), &mut self.fx),
        }
        let area = frame.area();
        self.fx.process(dt, frame.buffer_mut(), area);
    }

    fn render_header(&self, area: Rect, buf: &mut ratatui::buffer::Buffer) {
        let tab = |label: &'static str, t: Tab| {
            let style = if self.tab == t {
                Style::default().add_modifier(Modifier::REVERSED)
            } else {
                Style::default()
            };
            Span::styled(label, style)
        };
        Paragraph::new(Line::from(vec![
            tab(" 1 Graph ", Tab::Graph),
            Span::raw(" "),
            tab(" 2 List ", Tab::List),
            Span::raw("  Tab switch, q quit"),
        ]))
        .render(area, buf);
    }

    fn render_graph(&mut self, area: Rect, frame: &mut Frame) {
        let Some(flow) = &mut self.flow else { return };
        frame.render_widget(&mut *flow, area);
        for (id, color) in std::mem::take(&mut self.pending_glow) {
            if let Some(rect) = graph::node_rect(flow, &id, area) {
                self.fx.glow(&id, rect, color);
            }
        }
    }
}

struct TerminalGuard;

impl TerminalGuard {
    fn enter() -> io::Result<Self> {
        enable_raw_mode()?;
        execute!(io::stdout(), EnterAlternateScreen)?;
        let prev = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            restore();
            prev(info);
        }));
        Ok(Self)
    }
}

impl Drop for TerminalGuard {
    fn drop(&mut self) {
        restore();
    }
}

fn restore() {
    let _ = disable_raw_mode();
    let _ = execute!(io::stdout(), LeaveAlternateScreen);
}

fn dump(app: &mut App, w: u16, h: u16) {
    // A single frame at t=0 would show every effect's first (hidden) state.
    app.fx = Fx::new(true);
    let mut terminal = Terminal::new(TestBackend::new(w, h)).expect("TestBackend is infallible");
    terminal
        .draw(|f| app.draw(f, Duration::ZERO))
        .expect("TestBackend is infallible");
    let buf = terminal.backend().buffer();
    for y in 0..h {
        let row: String = (0..w)
            .map(|x| buf.cell((x, y)).map_or(" ", |c| c.symbol()))
            .collect();
        println!("{}", row.trim_end());
    }
}

fn main() -> io::Result<()> {
    let args = Args::parse();
    let mut app = App::new(&args);
    let (tx, rx) = mpsc::channel();

    let _live = if let Some(n) = args.synthetic {
        let (workers, roles) = graph::synth(n);
        app.set_graph_data(workers, roles);
        None
    } else if args.fixture.is_empty() {
        Some(source::spawn_live(&["map", "overview"], &tx)?)
    } else {
        source::spawn_fixture(
            args.fixture.clone(),
            Duration::from_millis(if args.dump.is_some() {
                0
            } else {
                args.replay_ms
            }),
            tx.clone(),
        );
        None
    };

    if let Some((w, h)) = args.dump {
        drop(tx);
        // Consume every document; the fixture thread ends the stream.
        for msg in rx {
            match msg {
                Msg::Doc(snap) => app.on_doc(snap),
                Msg::SourceEnded => break,
                Msg::Key(_) | Msg::Resize => {}
            }
        }
        dump(&mut app, w, h);
        return Ok(());
    }

    source::spawn_input(tx);
    let _guard = TerminalGuard::enter()?;
    let mut terminal = Terminal::new(CrosstermBackend::new(io::stdout()))?;
    run(&mut app, &mut terminal, &rx)
}

fn run(
    app: &mut App,
    terminal: &mut Terminal<CrosstermBackend<Stdout>>,
    rx: &mpsc::Receiver<Msg>,
) -> io::Result<()> {
    let mut last = Instant::now();
    let mut redraw = true;
    loop {
        if redraw {
            let now = Instant::now();
            let dt = now - last;
            last = now;
            app.list.tick(dt);
            terminal.draw(|f| app.draw(f, dt))?;
            redraw = false;
        }

        let timeout = match (app.effects_active(), app.animating_edges()) {
            (true, _) => Some(FX_TICK),
            (false, true) => Some(EDGE_TICK),
            (false, false) => None,
        };
        let msg = match timeout {
            None => match rx.recv() {
                Ok(m) => Some(m),
                Err(_) => return Ok(()),
            },
            Some(t) => match rx.recv_timeout(t) {
                Ok(m) => Some(m),
                Err(RecvTimeoutError::Timeout) => None,
                Err(RecvTimeoutError::Disconnected) => return Ok(()),
            },
        };
        if timeout.is_none() {
            // Nothing was animating: the sleep must not count as elapsed animation time.
            last = Instant::now();
        }

        match msg {
            Some(Msg::Doc(snap)) => {
                app.on_doc(snap);
                redraw = true;
            }
            Some(Msg::Key(k)) => {
                match k.code {
                    KeyCode::Char('q') => return Ok(()),
                    KeyCode::Char('c') if k.modifiers.contains(KeyModifiers::CONTROL) => {
                        return Ok(())
                    }
                    KeyCode::Tab => {
                        app.tab = if app.tab == Tab::Graph {
                            Tab::List
                        } else {
                            Tab::Graph
                        };
                    }
                    KeyCode::Char('1') => app.tab = Tab::Graph,
                    KeyCode::Char('2') => app.tab = Tab::List,
                    _ => continue,
                }
                redraw = true;
            }
            Some(Msg::Resize) => {
                terminal.autoresize()?;
                redraw = true;
            }
            Some(Msg::SourceEnded) => {}
            None => {
                if !app.reduced_motion {
                    if let Some(flow) = &mut app.flow {
                        flow.tick_animation(last.elapsed());
                    }
                }
                redraw = true;
            }
        }
    }
}
